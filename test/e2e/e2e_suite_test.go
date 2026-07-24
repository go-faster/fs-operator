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

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	// Some sandboxes give the build container no working DNS, which the Go
	// module proxy needs; DOCKER_BUILD_FLAGS=--network=host is the escape
	// hatch, and passing the environment through is what makes it reach make.
	cmd.Env = os.Environ()
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed(),
		"Failed to load the manager image into Kind")

	// The fs image is pulled once on the host and handed to the nodes: a
	// cluster that has to pull it per node turns a registry hiccup into a
	// flaky test.
	By("loading the fs image on Kind")
	Expect(loadFSImage()).To(Succeed(), "Failed to load the fs image into Kind")

	configureKubectlKubeRC()

	By("installing the operator from dist/chart")
	cmd = exec.Command("helm", "upgrade", "--install", releaseName, "dist/chart",
		"--namespace", namespace,
		"--create-namespace",
		"--set", "manager.image.repository="+imageRepository(managerImage),
		"--set", "manager.image.tag="+imageTag(managerImage),
		"--set", "manager.image.pullPolicy=IfNotPresent",
		"--wait", "--timeout", "5m",
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install the chart")
})

var _ = AfterSuite(func() {
	By("uninstalling the operator")
	cmd := exec.Command("helm", "uninstall", releaseName, "--namespace", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
})

// fsImage is the fs release the operator defaults to; it has to be in the
// cluster before a node can start.
const fsImage = "ghcr.io/go-faster/fs:v0.5.0"

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
