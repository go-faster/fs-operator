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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-faster/fs-operator/test/utils"
)

// The example gallery is documentation that executes, and SPEC §13 makes it
// part of "done": every example is exercised, so it cannot rot.
//
// Applying each one for real is not the bargain it looks like — a TLS example
// wants a certificate, an etcd-TLS example wants a CA and client keys, and the
// production example wants six nodes' worth of volumes. What actually decays
// under an example is its *validity*: the CRD schema, the CEL rules and the
// admission webhook all reject at apply time, and all three change without
// anyone opening examples/. A server-side dry run puts every one of them in
// the path, which is the check SPEC §13 names as the minimum.
//
// Two examples are also applied for real elsewhere in this suite (01 by the
// FSCluster container, 08 by the managed-etcd one). This covers the rest.
var _ = Describe("Examples", Ordered, func() {
	It("keeps every example applicable", func() {
		root, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred(), "could not locate the project directory")

		entries, err := os.ReadDir(filepath.Join(root, "examples"))
		Expect(err).NotTo(HaveOccurred(), "could not read the example gallery")

		var names []string

		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
				names = append(names, entry.Name())
			}
		}

		sort.Strings(names)

		// An empty gallery would let this spec pass while proving nothing —
		// the same trap as asserting against a namespace that was never
		// populated.
		Expect(names).NotTo(BeEmpty(), "the example gallery is empty")

		for _, name := range names {
			By("dry-run applying examples/" + name)

			// Server-side: the schema, the CEL rules and the webhook all run,
			// and nothing is created. No example pins a namespace, so they
			// are all validated against the same one.
			_, err := utils.Run(exec.Command("kubectl", "apply",
				"--dry-run=server", "-n", "default", "-f", "examples/"+name))
			Expect(err).NotTo(HaveOccurred(),
				"examples/%s is no longer accepted by the API", name)
		}
	})
})
