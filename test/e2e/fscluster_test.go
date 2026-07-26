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
	// clusterNamespace is where the fs cluster and its etcd run — a tenant
	// namespace, separate from the operator's.
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

// This is the end-to-end claim of P1: `kubectl apply` one FSCluster on a
// cluster with etcd produces a running fs cluster serving S3. Everything below
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

		By("deploying a three-member etcd")
		_, err := utils.Run(exec.Command("kubectl", "apply",
			"-n", clusterNamespace, "-f", "test/e2e/testdata/etcd.yaml"))
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy etcd")

		_, err = utils.Run(exec.Command("kubectl", "rollout", "status",
			"statefulset/etcd", "-n", clusterNamespace, "--timeout", "5m"))
		Expect(err).NotTo(HaveOccurred(), "etcd did not become ready")
	})

	AfterAll(func() {
		if CurrentSpecReport().Failed() {
			dumpCluster()
		}

		// Ask, but do not wait: emptying this namespace takes ~45s (three
		// StatefulSets, their PVCs, an etcd, and the cluster finalizer), and
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
	})

	It("serves S3", func() {
		stop := forwardS3()
		defer stop()

		client := s3Client()

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

		By("waiting for the bucket to be Ready with its scheme override")
		Eventually(func(g Gomega) {
			g.Expect(resourceCondition("fsbucket", "e2e-media", "Ready")).To(Equal("True"))
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		Expect(resourceField("fsbucket", "e2e-media", "{.status.scheme}")).To(Equal("rf3"),
			"the per-bucket scheme override did not take effect")

		By("waiting for the access key to be accepted by the cluster")
		Eventually(func(g Gomega) {
			g.Expect(resourceCondition("fsaccesskey", "e2e-writer", "Ready")).To(Equal("True"))
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("putting and getting an object with the minted credential")
		stop := forwardS3()
		defer stop()

		access := secretValue("e2e-writer-credentials", "access-key")
		secret := secretValue("e2e-writer-credentials", "secret-key")

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

	It("decommissions a node without losing data", func() {
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
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		victim := clusterName + "-3"
		Expect(nodeSets()).To(ContainElement(victim))

		By("dropping back to three, which decommissions the node just added")
		_, err = utils.Run(exec.Command("kubectl", "patch", "fscluster", clusterName,
			"-n", clusterNamespace, "--type", "merge",
			"-p", `{"spec":{"topology":{"nodes":3}}}`))
		Expect(err).NotTo(HaveOccurred(), "Failed to shrink the cluster")

		// Wait for the drain to actually start before waiting for it to finish.
		// Without this the next assertion could pass on status the operator has
		// not caught up with yet — and "the node is gone" would be satisfied by
		// a node that was never there.
		By("waiting for the operator to start draining it")
		Eventually(func(g Gomega) {
			g.Expect(resourceField("fscluster", clusterName, "{.status.update.phase}")).
				To(Equal("Draining"))
			g.Expect(resourceField("fscluster", clusterName, "{.status.update.node}")).
				To(Equal(victim))
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		// It is removed only once fs reports every one of its disks empty, so
		// this waits on a real drain rather than on a delete (SPEC §8.4). The
		// check is existence, not readiness: a node restarting onto the drained
		// config is briefly not ready and must not read as removed.
		By("waiting for the node to drain and be removed")
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
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		// The whole point: the object written before the decommission is still
		// readable after it.
		By("reading back an object written before the decommission")
		stop := forwardS3()
		defer stop()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		object, err := s3Client().GetObject(ctx, "e2e", "hello.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the decommission")

		defer func() { _ = object.Close() }()

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the decommission")
		Expect(got).To(Equal([]byte("fs-operator end-to-end")),
			"the object came back different after the decommission")
	})

	It("adds and removes a disk without losing data", func() {
		By("adding a second disk to every node")
		_, err := utils.Run(exec.Command("kubectl", "patch", "fscluster", clusterName,
			"-n", clusterNamespace, "--type", "merge",
			"-p", `{"spec":{"storage":{"disks":[{"name":"d0","size":"10Gi"},{"name":"d1","size":"10Gi"}]}}}`))
		Expect(err).NotTo(HaveOccurred(), "Failed to add the disk")

		// Adding a disk recreates each node's StatefulSet, one at a time, so
		// this waits on the whole roll rather than on the patch.
		By("waiting for every node to carry it")
		Eventually(func(g Gomega) {
			g.Expect(nodesWithDisk("d1")).To(Equal(3), "not every node has the new disk")
			g.Expect(readyNodes()).To(HaveLen(3))
			g.Expect(clusterCondition("Ready")).To(Equal("True"))
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("removing it again")
		_, err = utils.Run(exec.Command("kubectl", "patch", "fscluster", clusterName,
			"-n", clusterNamespace, "--type", "merge",
			"-p", `{"spec":{"storage":{"disks":[{"name":"d0","size":"10Gi"}]}}}`))
		Expect(err).NotTo(HaveOccurred(), "Failed to remove the disk")

		// The drain has to be observed starting, or "the disk is gone" below
		// would also be satisfied by a removal that never happened.
		By("waiting for the operator to start draining it")
		Eventually(func(g Gomega) {
			g.Expect(resourceField("fscluster", clusterName, "{.status.update.phase}")).
				To(Equal("Draining"))
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		// It leaves only once fs reports it empty on every node, so this waits
		// on a real drain rather than on a delete (SPEC §8.5).
		By("waiting for the disk to drain and be removed from every node")
		Eventually(func(g Gomega) {
			g.Expect(nodesWithDisk("d1")).To(BeZero(), "the disk is still on some node")
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("waiting for the cluster to settle without it")
		Eventually(func(g Gomega) {
			g.Expect(readyNodes()).To(HaveLen(3))
			g.Expect(clusterCondition("Ready")).To(Equal("True"))
			g.Expect(clusterCondition("ClusterSizeAligned")).To(Equal("True"))
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		// The point of draining rather than deleting: whatever the disk held
		// moved off it first.
		By("reading back an object written before the disk existed")
		stop := forwardS3()
		defer stop()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		object, err := s3Client().GetObject(ctx, "e2e", "hello.txt", minio.GetObjectOptions{})
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the disk removal")

		defer func() { _ = object.Close() }()

		got, err := io.ReadAll(object)
		Expect(err).NotTo(HaveOccurred(), "the object did not survive the disk removal")
		Expect(got).To(Equal([]byte("fs-operator end-to-end")),
			"the object came back different after the disk removal")
	})

	It("rejects an impossible spec at apply time", func() {
		// The webhook's whole purpose: the API server refuses the object, so
		// there is nothing to reconcile and nothing to clean up. Everything
		// else in this suite goes through the controller; this is the one
		// check that proves the admission path is wired — the Service selector
		// finds the manager, cert-manager's CA reached the configuration, and
		// the manager is serving TLS on 9443.
		By("applying a cluster whose topology cannot host its scheme")
		apply := exec.Command("kubectl", "apply", "-n", clusterNamespace, "-f", "-")
		apply.Stdin = strings.NewReader(strings.ReplaceAll(
			minimalExample(), "nodes: 3", "nodes: 2"))

		out, err := utils.Run(apply)
		Expect(err).To(HaveOccurred(), "the API server admitted an impossible spec")

		// The message a user reads has to name the reason, not just fail.
		Expect(out + errorText(err)).To(ContainSubstring("SchemeTopologyMismatch"))

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
// decommission is about existence: a node restarting onto its drained config is
// briefly not ready, and must not be mistaken for one that has been removed.
func nodeSets() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", clusterNamespace, "-l", "fs.go-faster.org/cluster="+clusterName,
		"-o", "jsonpath={.items[*].metadata.name}"))
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// nodesWithDisk counts the node StatefulSets whose claim templates declare a
// disk. Counting the templates is what says whether the operator has actually
// reshaped each node, which the PVCs cannot: they outlive their template under
// the default Retain policy.
func nodesWithDisk(disk string) int {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", clusterNamespace, "-l", "fs.go-faster.org/cluster="+clusterName,
		"-o", `jsonpath={range .items[*]}{range .spec.volumeClaimTemplates[*]}{.metadata.name} {end}{end}`))
	if err != nil {
		return -1
	}

	count := 0

	for _, name := range strings.Fields(out) {
		if name == disk {
			count++
		}
	}

	return count
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

// minimalExample is the published example, pointed at the e2e etcd. Only the
// endpoint changes: everything a reader of the docs would get, they get here.
func minimalExample() string {
	example, err := utils.Run(exec.Command("cat", "examples/01-minimal.yaml"))
	Expect(err).NotTo(HaveOccurred(), "Failed to read the example")

	return strings.ReplaceAll(example,
		"http://etcd.default.svc:2379",
		fmt.Sprintf("http://etcd.%s.svc:2379", clusterNamespace))
}

// tenancyManifest is an FSBucket and a generated FSAccessKey for the e2e
// cluster: rf3 (hostable on three nodes) as a per-bucket scheme override, and a
// write grant so the minted credential can round-trip an object.
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

// forwardS3 forwards the cluster's S3 Service to a local port and returns how
// to stop it.
func forwardS3() func() {
	forwardPort = freePort()

	cmd := exec.Command("kubectl", "port-forward", "-n", clusterNamespace,
		"service/"+clusterName, forwardPort+":8080")

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

// s3Client builds an S3 client from the cluster's generated root credentials —
// the same Secret an application would read.
func s3Client() *minio.Client {
	accessKey := secretValue(clusterName+"-root-credentials", "access-key")
	secretKey := secretValue(clusterName+"-root-credentials", "secret-key")

	client, err := minio.New("localhost:"+forwardPort, &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to build the S3 client")

	return client
}

// secretValue reads one key of a Secret in the tenant namespace.
func secretValue(name, key string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "secret", name,
		"-n", clusterNamespace, "-o", fmt.Sprintf("jsonpath={.data.%s}", key)))
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
