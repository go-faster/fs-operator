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

package fscluster

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"
)

// The controller tests run against a real API server (envtest): server-side
// apply, ownership, immutable fields and status subresources are exactly the
// parts a fake client would let pass. The environment starts on first use, so
// the rest of the package — renderers and builders, which need nothing —
// stays fast.

var (
	envOnce   sync.Once
	testEnv   *envtest.Environment
	k8sClient client.Client
	envErr    error
)

// eventBuffer is how many events a test recorder holds before it blocks.
const eventBuffer = 64

func TestMain(m *testing.M) {
	code := m.Run()

	if testEnv != nil {
		_ = testEnv.Stop()
	}

	os.Exit(code)
}

// reconciler starts the shared test environment and returns a reconciler
// wired to it. Tests that do not reach the API server never pay for it.
func reconciler(t *testing.T) (*Reconciler, *record.FakeRecorder) {
	t.Helper()

	envOnce.Do(startEnv)

	if envErr != nil {
		t.Fatalf("start test environment (run `make setup-envtest`): %v", envErr)
	}

	recorder := record.NewFakeRecorder(eventBuffer)

	return &Reconciler{
		Client:   k8sClient,
		Scheme:   scheme.Scheme,
		Recorder: recorder,
	}, recorder
}

// startEnv brings up the API server the controller tests share.
func startEnv() {
	if err := fsv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		envErr = err

		return
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envTestBinaryDir(),
	}

	cfg, err := testEnv.Start()
	if err != nil {
		envErr = err

		return
	}

	k8sClient, envErr = client.New(cfg, client.Options{Scheme: scheme.Scheme})
}

// envTestBinaryDir finds the API server binaries `make setup-envtest`
// installed, so the tests also run straight from an editor.
func envTestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")

	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}

	return ""
}
