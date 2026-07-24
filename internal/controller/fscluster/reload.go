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
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// Event reasons the reload step reports.
const (
	eventConfigReloaded = "ConfigReloaded"
	eventReloadFailed   = "ReloadFailed"
)

// reconcileReload brings every serving node to the desired configuration
// without a restart where fs can — the §8.3 fast path.
//
// A node whose pod template changed is (or will be) replaced by the rollout,
// and the fresh pod loads the new config; the reload step leaves those to the
// rollout. A node whose *only* change is hot-reloadable — credentials, grants,
// public-read buckets, the TLS certificate — keeps its pod: the operator bumps
// its config Secret (already applied this pass) and calls the admin reload
// endpoint, then confirms the node reports the target config revision. Kubelet
// Secret propagation lags, so a node that has not yet seen the new file still
// reports the old revision; the step requeues and tries again until every node
// converges (SPEC §8.3).
func (r *Reconciler) reconcileReload(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	log := logf.FromContext(ctx)

	token, err := r.adminToken(ctx, p)
	if err != nil {
		// The secrets step guarantees the token exists; a transient read error
		// here should not fail the whole pass. Leave the nodes uncounted and
		// come back.
		log.V(1).Info("Admin token unavailable; deferring reload verification", "error", err)

		return pipeline.RequeueAfter(pollInterval, "admin token unavailable")
	}

	var (
		configCurrent int32
		pending       bool
	)

	for i, node := range p.nodes {
		converged, wait := r.reloadNode(ctx, p, i, node, token)
		if converged {
			configCurrent++
		}

		pending = pending || wait
	}

	p.health.configCurrent = configCurrent

	if pending {
		return pipeline.RequeueAfter(pollInterval, "waiting for nodes to apply the desired configuration")
	}

	return pipeline.Continue()
}

// reloadNode converges one node's configuration, reporting whether it now runs
// the desired revision and whether the step should requeue for it.
func (r *Reconciler) reloadNode(ctx context.Context, p *pass, index int, node Node, token string) (converged, wait bool) {
	log := logf.FromContext(ctx)

	desired := p.configs[node.Name].Revision

	live, running := p.live[node.Name]
	if !running || !nodeServing(live) {
		// Not serving: it cannot be queried or reloaded now, and a fresh pod
		// loads the current config when it starts. Not yet converged.
		return false, true
	}

	// A node whose deployed pod template is not the desired one is being rolled
	// (or is about to be); the restart applies the new config, so leave it to
	// the rollout rather than reload a pod that is going away.
	if live.Annotations[AnnotationTemplateRevision] != p.desired[index].Annotations[AnnotationTemplateRevision] {
		return false, true
	}

	client, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, node.Name), token)
	if err != nil {
		log.V(1).Info("Admin client unavailable; will retry", "node", node.Name, "error", err)

		return false, true
	}

	info, err := client.Info(ctx)
	if err != nil {
		// An unreachable admin API is expected transiently (a node still
		// starting); requeue rather than fail.
		log.V(1).Info("Node admin API unreachable; will retry", "node", node.Name, "error", err)

		return false, true
	}

	if info.ConfigRevision == desired {
		return true, false
	}

	// Hot-only drift: the pod is the current template but its config is stale.
	// Reload and re-check; the Secret may not have propagated yet.
	res, err := client.Reload(ctx)
	if err != nil {
		r.Recorder.Eventf(p.object, corev1.EventTypeWarning, eventReloadFailed,
			"Reload of node %q failed: %v", node.Name, err)

		return false, true
	}

	if res.ConfigRevision == desired {
		r.Recorder.Eventf(p.object, corev1.EventTypeNormal, eventConfigReloaded,
			"Node %q reloaded to configuration %s", node.Name, desired)

		return true, false
	}

	// The node applied an older config than intended — the kubelet has not
	// propagated the new Secret to the mounted volume yet. Come back.
	log.V(1).Info("Node has not applied the target configuration yet",
		"node", node.Name, "applied", res.ConfigRevision, "target", desired)

	return false, true
}

// adminToken resolves the cluster's admin bearer token from its Secret.
func (r *Reconciler) adminToken(ctx context.Context, p *pass) (string, error) {
	var secret corev1.Secret

	name := AdminTokenSecretName(p.cluster.Name)
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.cluster.Namespace, Name: name}, &secret); err != nil {
		return "", errors.Wrapf(err, "get admin token secret %q", name)
	}

	token := string(secret.Data[AdminTokenKey])
	if token == "" {
		return "", errors.Errorf("admin token secret %q has no %q", name, AdminTokenKey)
	}

	return token, nil
}
