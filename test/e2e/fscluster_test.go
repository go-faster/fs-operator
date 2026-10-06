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
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/go-faster/fs-operator/test/utils"
)

const (
	// clusterNamespace is where the fs cluster runs — a tenant namespace,
	// separate from the operator's.
	clusterNamespace = "fs-e2e"

	// clusterName is the FSCluster of examples/01-minimal.yaml.
	clusterName = "fs-dev"
)

// forwardPort is the local port the S3 endpoint is currently forwarded to.
// Chosen per forward rather than fixed: a developer machine may already have
// something on any given port, and a forward that cannot bind is silent — the
// test then talks to whatever else owns it. That happened, and the symptom was
// an HTTP/2 frame arriving where an S3 response should be.
var forwardPort string

// This is the end-to-end claim of P1: `kubectl apply` one FSCluster produces a
// running fs cluster serving S3. Everything below
// goes through the same surfaces a user has — kubectl, the published example,
// the S3 API — and nothing reaches into the operator's internals.
var _ = Describe("FSCluster", Ordered, func() {
	BeforeAll(func() {
		By("creating the tenant namespace")
		// A previous run's namespace may still be terminating; wait it out so
		// the create does not race a delete (and the operator does not try to
		// write into a namespace being torn down).
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", clusterNamespace))

			return err
		}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
			"Failed to create the tenant namespace")
	})

	AfterAll(func() {
		if CurrentSpecReport().Failed() {
			dumpCluster()
		}

		// Ask, but do not wait: emptying this namespace takes a while (three
		// StatefulSets and their PVCs), and
		// nothing after this point needs it gone. The suite collects every
		// tenant namespace once, in SynchronizedAfterSuite, where the wait
		// overlaps the other containers instead of blocking them.
		By("deleting the tenant namespace")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", clusterNamespace,
			"--ignore-not-found", "--wait=false"))
	})

	It("provisions a cluster from the published example", func() {
		By("applying examples/01-minimal.yaml")
		apply := exec.Command("kubectl", "apply", "-n", clusterNamespace, "-f", "-")
		apply.Stdin = strings.NewReader(minimalExample())

		_, err := utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the example")

		By("waiting for every node's StatefulSet")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
				"-n", clusterNamespace, "-l", "fs.go-faster.org/cluster="+clusterName,
				"-o", "jsonpath={.items[*].status.readyReplicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(out)).To(HaveLen(3), "expected three node StatefulSets")

			for ready := range strings.FieldsSeq(out) {
				g.Expect(ready).To(Equal("1"), "a node is not ready")
			}
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("waiting for the cluster to report Ready")
		Eventually(func(g Gomega) {
			g.Expect(clusterCondition("Ready")).To(Equal("True"))
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		Expect(clusterCondition("SpecValid")).To(Equal("True"))
		Expect(clusterCondition("NodesHealthy")).To(Equal("True"))
		Expect(clusterCondition("ConfigurationInSync")).To(Equal("True"))

		// Ready needs a layout, which the operator applied once every node
		// was up in the cluster's own view.
		Expect(resourceField("fscluster", clusterName, "{.status.layout.members}")).To(Equal("3"))
	})

	It("serves S3", func() {
		stop := forwardS3(clusterNamespace, clusterName)
		defer stop()

		client := s3Client(clusterNamespace, clusterName)

		const bucket = "e2e"

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		By("creating a bucket")
		Expect(client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})).To(Succeed())

		By("putting an object")
		payload := []byte("fs-operator end-to-end")
		_, err := client.PutObject(ctx, bucket, "hello.txt",
			bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to put an object")

		By("getting it back")
		object, err := client.GetObject(ctx, bucket, "hello.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to get the object")

		defer func() { _ = object.Close() }()

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred(), "Failed to read the object")
		Expect(got).To(Equal(payload), "the object came back different")
	})

	It("serves a bucket and a generated access key", func() {
		By("applying an FSBucket and a generated FSAccessKey")
		apply := exec.Command("kubectl", "apply", "-n", clusterNamespace, "-f", "-")
		apply.Stdin = strings.NewReader(tenancyManifest())

		_, err := utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the tenancy resources")

		By("waiting for the bucket to be Ready with its scheme")
		Eventually(func(g Gomega) {
			g.Expect(resourceCondition("fsbucket", "e2e-media", "Ready")).To(Equal("True"))
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		Expect(resourceField("fsbucket", "e2e-media", "{.status.scheme}")).To(Equal("rf3"),
			"the per-bucket scheme did not take effect")

		By("waiting for every node to accept the access key")
		Eventually(func(g Gomega) {
			g.Expect(resourceCondition("fsaccesskey", "e2e-writer", "Ready")).To(Equal("True"))
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("putting and getting an object with the minted credential")
		stop := forwardS3(clusterNamespace, clusterName)
		defer stop()

		access := secretValue(clusterNamespace, "e2e-writer-credentials", "access-key")
		secret := secretValue(clusterNamespace, "e2e-writer-credentials", "secret-key")

		client, err := minio.New("localhost:"+forwardPort, &minio.Options{
			Creds: credentials.NewStaticV4(access, secret, ""),
		})
		Expect(err).NotTo(HaveOccurred(), "Failed to build the tenant S3 client")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		payload := []byte("fs-operator tenant object")
		_, err = client.PutObject(ctx, "e2e-media", "tenant.txt",
			bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "the minted credential could not put an object")

		object, err := client.GetObject(ctx, "e2e-media", "tenant.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "the minted credential could not get the object")

		defer func() { _ = object.Close() }()

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(payload), "the tenant object came back different")
	})

	It("removes a node without losing data", func() {
		By("growing the cluster to four nodes")
		_, err := utils.Run(exec.Command("kubectl", "patch", "fscluster", clusterName,
			"-n", clusterNamespace, "--type", "merge",
			"-p", `{"spec":{"topology":{"nodes":4}}}`))
		Expect(err).NotTo(HaveOccurred(), "Failed to grow the cluster")

		By("waiting for the fourth node to join")
		Eventually(func(g Gomega) {
			g.Expect(readyNodes()).To(HaveLen(4), "the fourth node did not join")
			g.Expect(clusterCondition("Ready")).To(Equal("True"))
			g.Expect(clusterCondition("ClusterSizeAligned")).To(Equal("True"))
			g.Expect(resourceField("fscluster", clusterName, "{.status.layout.members}")).To(Equal("4"))
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		victim := clusterName + "-3"
		Expect(nodeSets()).To(ContainElement(victim))

		By("dropping back to three, which removes the node just added")
		_, err = utils.Run(exec.Command("kubectl", "patch", "fscluster", clusterName,
			"-n", clusterNamespace, "--type", "merge",
			"-p", `{"spec":{"topology":{"nodes":3}}}`))
		Expect(err).NotTo(HaveOccurred(), "Failed to shrink the cluster")

		// Wait for the removal to actually start before waiting for it to finish.
		// Without this the next assertion could pass on status the operator has
		// not caught up with yet — and "the node is gone" would be satisfied by
		// a node that was never there.
		By("waiting for the operator to take it out of the layout")
		Eventually(func(g Gomega) {
			g.Expect(resourceField("fscluster", clusterName, "{.status.layout.members}")).To(Equal("3"))
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		// It is deleted only once the layout change that moves its data has
		// completed — no older version retained — so this waits on a real
		// transition rather than on a delete (SPEC §8.4).
		By("waiting for its data to move and the node to be removed")
		Eventually(func(g Gomega) {
			g.Expect(nodeSets()).NotTo(ContainElement(victim), "the node is still there")
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("waiting for the cluster to settle without it")
		Eventually(func(g Gomega) {
			g.Expect(nodeSets()).To(HaveLen(3))
			g.Expect(readyNodes()).To(HaveLen(3))
			g.Expect(clusterCondition("Ready")).To(Equal("True"))
			g.Expect(clusterCondition("ClusterSizeAligned")).To(Equal("True"))
			g.Expect(clusterCondition("SpecValid")).To(Equal("True"))
			g.Expect(resourceField("fscluster", clusterName, "{.status.layout.retainedVersions}")).To(BeEmpty())
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		// The whole point: the object written before the removal is still
		// readable after it.
		By("reading back an object written before the removal")
		stop := forwardS3(clusterNamespace, clusterName)
		defer stop()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		object, err := s3Client(clusterNamespace, clusterName).GetObject(ctx, "e2e", "hello.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the removal")

		defer func() { _ = object.Close() }()

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the removal")
		Expect(got).To(Equal([]byte("fs-operator end-to-end")),
			"the object came back different after the removal")
	})

	It("rejects an impossible spec at apply time", func() {
		// The webhook's whole purpose: the API server refuses the object, so
		// there is nothing to reconcile and nothing to clean up. Everything
		// else in this suite goes through the controller; this is the one
		// check that proves the admission path is wired — the Service selector
		// finds the manager, cert-manager's CA reached the configuration, and
		// the manager is serving TLS on 9443.
		// A width wider than the cluster passes the CRD's own rules — CEL sees
		// one field at a time — so only the webhook can refuse it.
		By("applying a cluster too small for its layout width")
		apply := exec.Command("kubectl", "apply", "-n", clusterNamespace, "-f", "-")
		apply.Stdin = strings.NewReader(`apiVersion: fs.go-faster.org/v1alpha1
kind: FSCluster
metadata:
  name: fs-impossible
spec:
  topology:
    nodes: 3
  storage:
    size: 1Gi
  layout:
    widths: [3, 6]
`)

		out, err := utils.Run(apply)
		Expect(err).To(HaveOccurred(), "the API server admitted an impossible spec")

		// The message a user reads has to name the reason, not just fail.
		Expect(out + errorText(err)).To(ContainSubstring("LayoutTopologyMismatch"))

		By("checking the running cluster was not touched")
		Expect(nodeSets()).To(HaveLen(3))
	})

})

// errorText is an error's message, for asserting on what kubectl printed.
func errorText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// nodeSets names every node StatefulSet the cluster has, ready or not. A
// removal is about existence: a node that is briefly not ready must not be
// mistaken for one that has been removed.
func nodeSets() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", clusterNamespace, "-l", "fs.go-faster.org/cluster="+clusterName,
		"-o", "jsonpath={.items[*].metadata.name}"))
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// readyNodes names the cluster's node StatefulSets that report their pod ready.
func readyNodes() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", clusterNamespace, "-l", "fs.go-faster.org/cluster="+clusterName,
		"-o", `jsonpath={range .items[?(@.status.readyReplicas==1)]}{.metadata.name} {end}`))
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// minimalExample is the published example, exactly as a reader of the docs
// gets it.
func minimalExample() string {
	example, err := utils.Run(exec.Command("cat", "examples/01-minimal.yaml"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the example")

	return example
}

// tenancyManifest is an FSBucket and a generated FSAccessKey for the e2e
// cluster: rf3 (hostable on three nodes) as the bucket's scheme, and a write
// grant so the minted credential can round-trip an object.
func tenancyManifest() string {
	return fmt.Sprintf(`apiVersion: fs.go-faster.org/v1alpha1
kind: FSBucket
metadata:
  name: e2e-media
spec:
  clusterRef:
    name: %[1]s
  scheme: rf3
---
apiVersion: fs.go-faster.org/v1alpha1
kind: FSAccessKey
metadata:
  name: e2e-writer
spec:
  clusterRef:
    name: %[1]s
  grants:
    - bucket: "e2e-media"
      permission: write
`, clusterName)
}

// resourceCondition reads one status condition of a namespaced resource.
func resourceCondition(kind, name, conditionType string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", kind, name, "-n", clusterNamespace,
		"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==%q)].status}", conditionType)))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(out)
}

// resourceField reads a jsonpath field of a namespaced resource.
func resourceField(kind, name, jsonpath string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", kind, name, "-n", clusterNamespace,
		"-o", "jsonpath="+jsonpath))
	Expect(err).NotTo(HaveOccurred())

	return strings.TrimSpace(out)
}

// clusterCondition reads one condition of the cluster.
func clusterCondition(conditionType string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "fscluster", clusterName,
		"-n", clusterNamespace,
		"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==%q)].status}", conditionType)))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(out)
}

// forwardS3 forwards a cluster's S3 Service to a local port and returns how to
// stop it.
func forwardS3(namespace, cluster string) func() {
	forwardPort = freePort()

	cmd := exec.Command("kubectl", "port-forward", "-n", namespace,
		"service/"+cluster, forwardPort+":8080")

	Expect(cmd.Start()).To(Succeed(), "Failed to start the port forward")

	// Wait for the forward itself to answer, not for the Service to exist.
	// kubectl port-forward that cannot bind exits quietly, and every later
	// request then goes to whoever does own the port.
	Eventually(func() error {
		conn, err := net.DialTimeout("tcp", "localhost:"+forwardPort, 2*time.Second)
		if err != nil {
			return err
		}

		return conn.Close()
	}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed(),
		"the S3 port forward never started serving")

	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// freePort reserves a local port the forward can have to itself.
func freePort() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred(), "Failed to reserve a local port")

	_, port, err := net.SplitHostPort(listener.Addr().String())
	Expect(err).NotTo(HaveOccurred())
	Expect(listener.Close()).To(Succeed())

	return port
}

// s3Client builds an S3 client from a cluster's generated root credentials —
// the same Secret an application would read.
func s3Client(namespace, cluster string) *minio.Client {
	accessKey := secretValue(namespace, cluster+"-root-credentials", "access-key")
	secretKey := secretValue(namespace, cluster+"-root-credentials", "secret-key")

	client, err := minio.New("localhost:"+forwardPort, &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to build the S3 client")

	return client
}

// secretValue reads one key of a Secret in a tenant namespace.
func secretValue(namespace, name, key string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "secret", name,
		"-n", namespace, "-o", fmt.Sprintf("jsonpath={.data.%s}", key)))
	Expect(err).NotTo(HaveOccurred(), "Failed to read secret %q", name)

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	Expect(err).NotTo(HaveOccurred(), "Failed to decode secret %q", name)

	return string(decoded)
}

// dumpCluster prints what a failing run needs to be diagnosed from CI logs.
func dumpCluster() {
	const get = "get"

	for _, args := range [][]string{
		{get, "fscluster", "-n", clusterNamespace, "-o", "yaml"},
		{get, "pods", "-n", clusterNamespace, "-o", "wide"},
		{get, "events", "-n", clusterNamespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", clusterNamespace, "-l", "app.kubernetes.io/name=fs", "--tail", "100", "--prefix"},
		{"logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail", "200"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		if err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n$ kubectl %s\n%s\n", strings.Join(args, " "), out)
		}
	}
}
