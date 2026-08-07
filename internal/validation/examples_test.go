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

package validation_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
	"github.com/go-faster/fs-operator/internal/validation"
)

// TestExamplesAreAccepted runs every published example through the checks the
// webhook and the controller both run.
//
// The e2e suite already dry-run applies the gallery, but that needs a cluster
// with the webhook enabled, so it says nothing on a laptop or in a unit run.
// This is the cheap half of the same claim: an example the operator would
// refuse is a `kubectl apply` from the README that ends in a warning event.
func TestExamplesAreAccepted(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}

		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var clusters int

			// An example may hold several documents (a cluster plus its
			// buckets and keys); only the clusters are this package's.
			for doc := range bytes.SplitSeq(data, []byte("\n---")) {
				var cluster fsv1alpha1.FSCluster

				if err := yaml.Unmarshal(doc, &cluster); err != nil {
					t.Fatalf("parse: %v", err)
				}

				if cluster.Kind != "FSCluster" {
					continue
				}

				clusters++

				spec := cluster.Spec
				spec.WithDefaults()

				if failure := validation.Cluster(&spec); failure != nil {
					t.Errorf("%s is refused: %s (%s)", cluster.Name, failure.Message, failure.Reason)
				}

				for _, warning := range validation.ClusterWarnings(&spec) {
					t.Logf("%s warns: %s", cluster.Name, strings.SplitN(warning, ":", 2)[0])
				}
			}

			if clusters == 0 && strings.Contains(string(data), "kind: FSCluster") {
				t.Error("the example declares a cluster this test did not check")
			}
		})
	}
}
