//go:build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/minio/minio-go/v7"

	"github.com/go-faster/fs-operator/test/utils"
)

const (
	// singleNamespace holds the single-node cluster. Its own namespace,
	// because the claim is partly about what is *not* in it.
	singleNamespace = "fs-e2e-single"

	// singleCluster is the FSCluster of examples/00-single-node.yaml.
	singleCluster = "fs-dev"
)

// Single-node mode renders a configuration nothing else in the suite does —
// fs's filesystem backend, no cluster section, no etcd — and the only way to
// know fs accepts it is to start fs with it. Everything here goes through the
// surfaces a developer has: the published example, kubectl, and S3.
var _ = Describe("Single-node cluster", Ordered, func() {
	BeforeAll(func() {
		By("creating the tenant namespace")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", singleNamespace))

			return err
		}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
			"Failed to create the tenant namespace")
	})

	AfterAll(func() {
		if CurrentSpecReport().Failed() {
			dumpSingleNode()
		}

		By("deleting the tenant namespace")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", singleNamespace,
			"--ignore-not-found", "--wait=false"))
	})

	It("brings up one node with no etcd", func() {
		By("applying examples/00-single-node.yaml")
		apply := exec.Command("kubectl", "apply", "-n", singleNamespace, "-f",
			"examples/00-single-node.yaml")

		_, err := utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the single-node example")

		By("waiting for the node's StatefulSet")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
				"-n", singleNamespace, "-o", "jsonpath={.items[*].status.readyReplicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			// One StatefulSet, and it is the node's: a managed etcd would be
			// a second one here.
			g.Expect(strings.Fields(out)).To(HaveLen(1), "expected exactly one StatefulSet")
			g.Expect(strings.TrimSpace(out)).To(Equal("1"), "the node is not ready")
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		// Ready without a quorum of failure domains is the whole point: the
		// one node serving is the whole cluster.
		By("waiting for the cluster to report Ready")
		Eventually(func(g Gomega) {
			g.Expect(singleNodeCondition("Ready")).To(Equal("True"))
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		Expect(singleNodeCondition("SpecValid")).To(Equal("True"))
		Expect(singleNodeCondition("NodesHealthy")).To(Equal("True"))
		Expect(singleNodeCondition("ConfigurationInSync")).To(Equal("True"))
	})

	It("serves S3", func() {
		stop := forwardS3(singleNamespace, singleCluster)
		defer stop()

		client := s3Client(singleNamespace, singleCluster)

		const bucket = "solo"

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		By("creating a bucket")
		Expect(client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})).To(Succeed())

		By("putting an object")
		payload := []byte("fs-operator single node")
		_, err := client.PutObject(ctx, bucket, "hello.txt",
			bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to put an object")

		By("getting it back")
		object, err := client.GetObject(ctx, bucket, "hello.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to get the object")

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred(), "Failed to read the object")
		Expect(got).To(Equal(payload))
	})

	It("says it is development only", func() {
		// The shape is permanently unsafe for data anyone keeps, so it has to
		// be visible on the object and not only in the docs (SPEC §5.2).
		By("looking for the warning event")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "events",
				"-n", singleNamespace,
				"--field-selector", "reason=UnsupportedTopology",
				"-o", "jsonpath={.items[*].message}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("development only"))
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})

	It("refuses to be grown into a cluster", func() {
		// The backends store data differently, so raising the node count in
		// place would come back as an empty cluster (SPEC §5.2).
		By("patching the node count")
		patch := exec.Command("kubectl", "patch", "fscluster", singleCluster,
			"-n", singleNamespace, "--type", "merge",
			"-p", `{"spec":{"topology":{"nodes":3}}}`)

		out, err := utils.Run(patch)
		Expect(err).To(HaveOccurred(), "the API server admitted a backend switch")
		Expect(out + errorText(err)).To(ContainSubstring("single-node"))

		By("checking the cluster is untouched")
		Expect(singleNodeCondition("Ready")).To(Equal("True"))
	})
})

// singleNodeCondition reads one condition status off the single-node cluster.
func singleNodeCondition(conditionType string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "fscluster", singleCluster,
		"-n", singleNamespace,
		"-o", `jsonpath={.status.conditions[?(@.type=="`+conditionType+`")].status}`))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(out)
}

// dumpSingleNode prints what a failing run needs to be diagnosed from CI logs.
func dumpSingleNode() {
	for _, args := range [][]string{
		{"get", "fscluster", "-n", singleNamespace, "-o", "yaml"},
		{"get", "pods", "-n", singleNamespace, "-o", "wide"},
		{"get", "events", "-n", singleNamespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", singleNamespace, "-l", "fs.go-faster.org/cluster=" + singleCluster,
			"--tail", "200"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		if err == nil {
			GinkgoWriter.Printf("=== kubectl %s ===\n%s\n", strings.Join(args, " "), out)
		}
	}
}
