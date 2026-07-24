#!/usr/bin/env bash
#
# Sync the owned Helm chart from the authoritative generated sources. The
# chart is committed and hand-owned, so everything it ships that is generated
# elsewhere — the CRDs (config/crd) and the manager's RBAC rules
# (config/rbac/role.yaml) — must be kept in lockstep rather than left to
# drift. A drifted manager role is not a lint failure: it is an operator that
# starts, cannot list the resources it owns, and silently never reconciles.
#
# Run after `make manifests` (which regenerates config/crd and config/rbac
# from the API types and the +kubebuilder:rbac markers); `make helm-sync-crds`
# does both. CI fails when a chart copy is stale.

set -euo pipefail

sync_crds() {
	local src_dir="config/crd/bases"
	local dst_dir="dist/chart/templates/crd"

	mkdir -p "$dst_dir"

	local src base group plural dst
	for src in "$src_dir"/*.yaml; do
		# fs.go-faster.org_fsclusters.yaml -> fsclusters.fs.go-faster.org.yaml
		base="$(basename "$src" .yaml)"
		group="${base%%_*}"
		plural="${base#*_}"
		dst="$dst_dir/$plural.$group.yaml"

		{
			echo '{{- if .Values.crd.enabled }}'
			# Drop the leading YAML document separator and inject the
			# keep-policy guard immediately after metadata.annotations.
			awk '
				NR == 1 && $0 == "---" { next }
				{ print }
				/^  annotations:$/ && !injected {
					print "    {{- if .Values.crd.keep }}"
					print "    \"helm.sh/resource-policy\": keep"
					print "    {{- end }}"
					injected = 1
				}
			' "$src"
			echo '{{- end }}'
		} >"$dst"

		echo "synced $dst from $src"
	done
}

# sync_manager_role rewrites the chart's manager-role rules from the generated
# ClusterRole, keeping the chart's templated header (namespaced Role vs
# ClusterRole, resource naming) that the plugin scaffolded and owns.
sync_manager_role() {
	local src="config/rbac/role.yaml"
	local dst="dist/chart/templates/rbac/manager-role.yaml"

	{
		# The header down to and including the `rules:` line is the chart's;
		# preserve it verbatim.
		awk '/^rules:/ { print; exit } { print }' "$dst"

		# The rules are the generated role's, from its own `rules:` line to
		# the end. The generated file has no chart templating to preserve.
		awk 'found { print } /^rules:/ { found = 1 }' "$src"
	} >"$dst.tmp"

	mv "$dst.tmp" "$dst"

	echo "synced $dst from $src"
}

sync_crds
sync_manager_role
