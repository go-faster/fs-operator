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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-faster/fs-operator/test/utils"
)

const (
	// managedNamespace is where the self-contained cluster runs. It gets its
	// own namespace because the point is that nothing else is in it.
	managedNamespace = "fs-e2e-managed"

	// managedCluster is the FSCluster of examples/08-managed-etcd.yaml.
	managedCluster = "fs-dev"
)

// The managed etcd is the one part of the operator whose failure modes are all
// outside envtest: whether etcd starts with the arguments the builder writes,
// under a read-only root filesystem as a non-root user, and whether the fs
// nodes can reach it at the DNS names the config renderer generates. So it is
// tested the way a user meets it — apply the published example, and nothing
// else.
var _ = Describe("Managed etcd", Ordered, func() {
	BeforeAll(func() {
		By("creating the tenant namespace")
		Eventually(func() error {
			_, err := utils.Run(exec.Command("kubectl", "create", "ns", managedNamespace))

			return err
		}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
			"Failed to create the tenant namespace")
	})

	AfterAll(func() {
		if CurrentSpecReport().Failed() {
			dumpManagedCluster()
		}

		// Asked for here, waited for in SynchronizedAfterSuite; see the note in
		// the FSCluster container's AfterAll.
		By("deleting the tenant namespace")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", managedNamespace,
			"--ignore-not-found", "--wait=false"))
	})

	It("brings up a cluster with no external etcd", func() {
		By("applying examples/08-managed-etcd.yaml")
		apply := exec.Command("kubectl", "apply", "-n", managedNamespace, "-f",
			"examples/08-managed-etcd.yaml")

		_, err := utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply the managed-etcd example")

		By("waiting for the operator's etcd")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
				managedCluster+"-etcd", "-n", managedNamespace,
				"-o", "jsonpath={.status.readyReplicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("1"), "the managed etcd is not ready")
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		// The nodes reaching Ready is the actual claim: they were configured
		// with the managed members' DNS names, registered in that etcd, and
		// formed a cluster. Asserted on the nodes as well as the condition, so
		// this cannot pass on a cluster that never built any.
		By("waiting for the fs cluster to report Ready")
		Eventually(func(g Gomega) {
			g.Expect(managedNodes()).To(HaveLen(3), "the fs nodes are not ready")

			out, err := utils.Run(exec.Command("kubectl", "get", "fscluster", managedCluster,
				"-n", managedNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("True"))
		}).WithTimeout(10 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
	})

	It("warns that the managed etcd is development only", func() {
		// A permanent property of the mode, so it has to be visible on the
		// object rather than only in the docs (SPEC §2).
		By("looking for the warning event")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "events",
				"-n", managedNamespace,
				"--field-selector", "reason=ManagedEtcdUnsupported",
				"-o", "jsonpath={.items[*].message}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("development only"))
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})

	It("takes its etcd down with the cluster", func() {
		// Assert it is all there first. Without this the check below is
		// satisfied by a namespace that never had anything in it.
		By("checking the etcd and nodes exist to begin with")
		Expect(managedStatefulSets()).To(ContainElement(managedCluster+"-etcd"),
			"the managed etcd is not there to be deleted")
		Expect(managedStatefulSets()).To(HaveLen(4), "want three fs nodes plus the etcd")

		By("deleting the FSCluster")
		_, err := utils.Run(exec.Command("kubectl", "delete", "fscluster", managedCluster,
			"-n", managedNamespace, "--timeout", "5m"))
		Expect(err).NotTo(HaveOccurred(), "Failed to delete the cluster")

		// Owned by the cluster, so garbage collection takes it — and with the
		// default reclaim policy its volumes go too, which is what keeps a
		// re-created cluster from adopting a stale key store (SPEC §8.6).
		By("checking the etcd went with it")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
				"-n", managedNamespace, "-o", "jsonpath={.items[*].metadata.name}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(out)).To(BeEmpty(), "something outlived the cluster")
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	})
})

// managedStatefulSets names every StatefulSet in the managed namespace, ready
// or not.
func managedStatefulSets() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", managedNamespace, "-o", "jsonpath={.items[*].metadata.name}"))
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// managedNodes names the fs node StatefulSets reporting their pod ready.
//
// Selected on app.kubernetes.io/name, not on the cluster label: the etcd
// carries the cluster label too, and selecting on that would count it as a
// fourth node. That the app name separates them is the same property the
// client Service and the disruption budget depend on.
func managedNodes() []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "statefulset",
		"-n", managedNamespace,
		"-l", "app.kubernetes.io/name=fs,fs.go-faster.org/cluster="+managedCluster,
		"-o", `jsonpath={range .items[?(@.status.readyReplicas==1)]}{.metadata.name} {end}`))
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// dumpManagedCluster prints what a failing managed-etcd spec needs explained.
func dumpManagedCluster() {
	for _, args := range [][]string{
		{"get", "fscluster", "-n", managedNamespace, "-o", "yaml"},
		{"get", "pods,statefulsets,pvc", "-n", managedNamespace},
		{"logs", "-n", managedNamespace, "statefulset/" + managedCluster + "-etcd", "--tail", "50"},
		{"get", "events", "-n", managedNamespace, "--sort-by", ".lastTimestamp"},
	} {
		out, _ := utils.Run(exec.Command("kubectl", args...))
		_, _ = GinkgoWriter.Write([]byte(strings.Join(args, " ") + ":\n" + out + "\n"))
	}
}
