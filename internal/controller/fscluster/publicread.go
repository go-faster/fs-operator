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
	"slices"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-faster/fs-operator/internal/controller/pipeline"
)

// reconcilePublicRead reconciles the cluster-wide anonymously-readable bucket
// list (spec.auth.publicReadBuckets) through the admin API. Under the etcd
// credential store the list lives in etcd, sealed and hot-reloaded on every
// node (fs §6.8), so — like access keys — the operator manages it at runtime
// rather than rendering it into the config.
func (r *Reconciler) reconcilePublicRead(ctx context.Context, p *pass) (pipeline.Outcome, error) {
	log := logf.FromContext(ctx)

	serving := servingNodes(p)
	if len(serving) == 0 {
		// No node is up to accept the list yet; a later pass applies it.
		return pipeline.Continue()
	}

	token, err := r.adminToken(ctx, p)
	if err != nil {
		return pipeline.RequeueAfter(pollInterval, "admin token unavailable")
	}

	admin, err := r.adminClient(AdminURL(p.cluster.Name, p.cluster.Namespace, serving[0].Name), token)
	if err != nil {
		return pipeline.RequeueAfter(pollInterval, "admin client unavailable")
	}

	current, err := admin.GetPublicReadBuckets(ctx)
	if err != nil {
		// The cluster is not reachable yet; try again shortly.
		return pipeline.RequeueAfter(pollInterval, "public-read list unavailable")
	}

	desired := p.cluster.Spec.Auth.PublicReadBuckets

	if samePublicRead(current, desired) {
		return pipeline.Continue()
	}

	if err := admin.SetPublicReadBuckets(ctx, desired); err != nil {
		return pipeline.RequeueAfter(pollInterval, "setting the public-read list failed")
	}

	r.Recorder.Eventf(p.object, corev1.EventTypeNormal, "PublicReadUpdated",
		"set the cluster public-read list to %d bucket(s)", len(desired))
	log.V(1).Info("Reconciled public-read buckets", "count", len(desired))

	return pipeline.Continue()
}

// samePublicRead reports whether two public-read lists hold the same set of
// buckets, order-independent.
func samePublicRead(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	as := slices.Clone(a)
	bs := slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)

	return slices.Equal(as, bs)
}
