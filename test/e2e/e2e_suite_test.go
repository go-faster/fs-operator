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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-faster/fs-operator/test/utils"
)

const (
	// managerImage is the operator image built and loaded for the tests.
	managerImage = "example.com/fs-operator:v0.0.1"

	// releaseName is the Helm release; it also names the operator's
	// resources, so it is what the manager specs look for.
	releaseName = "fs-operator"

	// namespace is where the operator runs.
	namespace = "fs-operator-system"
)

// tenantNamespaces are the per-container namespaces the suite creates, each
// owned by one top-level container and torn down asynchronously by its
// AfterAll. The suite waits for them together at the end.
var tenantNamespaces = []string{clusterNamespace, managedNamespace}

// TestE2E runs the e2e suite against a Kind cluster.
//
// The operator is installed the way a user installs it: the owned Helm chart
// in dist/chart (SPEC §14). If the chart cannot deploy the operator, no test
// below can pass — which is the point of not using a test-only manifest path.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting fs-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

// The suite runs its top-level containers in parallel, so this setup has to
// happen exactly once no matter how many processes there are: building an image
// three times is waste, but installing cert-manager or the chart three times
// concurrently is a race. SynchronizedBeforeSuite runs the first function on
// process 1 alone and blocks the others until it returns.
var _ = SynchronizedBeforeSuite(func() []byte {
	// Before any kubectl call, including the ones below.
	configureKubectlKubeRC()

	// The build talks to docker and everything below it talks to the cluster,
	// so the two have no reason to take turns: the build is the single longest
	// step here and the cluster-side setup is almost all waiting. Start it now
	// and collect it once there is something that actually needs the image.
	By("building the manager image")

	built := make(chan error, 1)

	go func() {
		defer GinkgoRecover()

		cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
		// Some sandboxes give the build container no working DNS, which the Go
		// module proxy needs; DOCKER_BUILD_FLAGS=--network=host is the escape
		// hatch, and passing the environment through is what makes it reach make.
		cmd.Env = os.Environ()

		_, err := utils.Run(cmd)
		built <- err
	}()

	// The suite starts as soon as the kind cluster answers, which can be
	// before CoreDNS is serving. A spec that resolves a Service name then
	// fails with "could not resolve host" — observed once, on the metrics
	// spec, five minutes before the same cluster resolved everything else
	// fine. Waiting here costs nothing and removes a whole class of flake.
	By("waiting for cluster DNS")
	_, err := utils.Run(exec.Command("kubectl", "rollout", "status",
		"deployment/coredns", "-n", "kube-system", "--timeout", "5m"))
	Expect(err).NotTo(HaveOccurred(), "CoreDNS did not become ready")

	// The admission webhook needs a certificate the API server trusts, so the
	// suite installs cert-manager and turns the webhook on. It is the half of
	// the webhook unit tests cannot reach: the Service selector matching the
	// manager pod, the CA bundle being injected, and the manager actually
	// serving TLS on 9443.
	//
	// It goes before the chart, which asks for an Issuer and a Certificate.
	By("installing cert-manager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install cert-manager")

	Expect(<-built).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed(),
		"Failed to load the manager image into Kind")

	// The fs image is pulled once on the host and handed to the nodes: a
	// cluster that has to pull it per node turns a registry hiccup into a
	// flaky test.
	By("loading the fs image on Kind")
	Expect(loadFSImage()).To(Succeed(), "Failed to load the fs image into Kind")

	By("installing the operator from dist/chart")
	cmd := exec.Command("helm", "upgrade", "--install", releaseName, "dist/chart",
		"--namespace", namespace,
		"--create-namespace",
		"--set", "manager.image.repository="+imageRepository(managerImage),
		"--set", "manager.image.tag="+imageTag(managerImage),
		"--set", "manager.image.pullPolicy=IfNotPresent",
		"--set", "webhook.enabled=true",
		"--set", "certManager.enabled=true",
		"--wait", "--timeout", "5m",
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install the chart")

	// helm --wait returns the moment the pod reports Ready, which is when the
	// webhook server has bound its port — but the Service backend is
	// programmed a beat later, and until it is the ClusterIP rejects with
	// "connection refused". With failurePolicy: Fail that turns the first
	// apply after an install into a coin flip, so wait for the path to
	// actually work rather than for its parts to exist.
	//
	// A server-side dry run goes through admission without creating anything,
	// which is exactly the question being asked.
	By("waiting for the webhook to accept requests")
	Eventually(func(g Gomega) {
		probe := exec.Command("kubectl", "apply", "--dry-run=server", "-n", "default", "-f", "-")
		probe.Stdin = strings.NewReader(minimalExample())

		_, err := utils.Run(probe)
		g.Expect(err).NotTo(HaveOccurred())
	}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(Succeed(),
		"the admission webhook never became reachable")

	return nil
}, func(_ []byte) {
	// Every process: KUBECTL_KUBERC is process state, so setting it on process
	// 1 leaves the others reading the developer's own kuberc.
	configureKubectlKubeRC()
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	// Process 1, after every other process has finished.
	//
	// Each container's AfterAll asks for its namespace to go but does not wait
	// (see the note there). What has to happen before the operator is
	// uninstalled is narrower than the whole namespace: an FSCluster carries a
	// finalizer only the operator clears, so uninstalling with one still around
	// strands the object, and the namespace holding it, in Terminating for
	// ever.
	//
	// The rest of the namespace is not worth waiting for. Terminating pods and
	// releasing PVCs are the bulk of the ~45s it takes, and neither needs an
	// operator to finish — so let them run down on their own, whether that is
	// under a kind cluster about to be deleted or before the next run.
	By("waiting for the FSClusters to finalize")

	for _, ns := range tenantNamespaces {
		_, _ = utils.Run(exec.Command("kubectl", "wait", "--for=delete",
			"fsclusters", "--all", "-n", ns, "--timeout=5m"))
	}

	By("uninstalling the operator")

	cmd := exec.Command("helm", "uninstall", releaseName, "--namespace", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
})

// fsImage is the fs release the operator defaults to; it has to be in the
// cluster before a node can start.
const fsImage = "ghcr.io/go-faster/fs:v0.13.1"

// loadFSImage pulls the fs image on the host and loads it into Kind.
func loadFSImage() error {
	if _, err := utils.Run(exec.Command("docker", "pull", fsImage)); err != nil {
		return err
	}

	return utils.LoadImageToKindClusterWithName(fsImage)
}

// imageRepository and imageTag split a reference for the chart's values, at
// the last colon so a registry port does not look like a tag.
func imageRepository(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[:i]
	}

	return image
}

func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}

	return ""
}

// configureKubectlKubeRC disables kubectl kuberc by default, so a developer's
// local kubectl configuration cannot change what the tests do.
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")

		return
	}

	By("disabling kubectl kuberc for test isolation")
	Expect(os.Setenv("KUBECTL_KUBERC", "false")).To(Succeed(), "Failed to disable kubectl kuberc")
}
