#!/usr/bin/env bash
#
# Sync the owned Helm chart's CRD templates from the authoritative
# config/crd source. The chart is committed and hand-owned, so the CRDs it
# ships must be kept in lockstep with config/crd rather than drifting.
#
# Each chart CRD is the config/crd document wrapped in the chart's
# crd.enabled guard, with a helm.sh/resource-policy: keep annotation gated on
# crd.keep — the same shape the helm plugin scaffolds.
#
# Run after `make manifests` (which regenerates config/crd from the API
# types); `make helm-sync-crds` does both. CI fails when the chart copy is
# stale.

set -euo pipefail

SRC_DIR="config/crd/bases"
DST_DIR="dist/chart/templates/crd"

mkdir -p "$DST_DIR"

for src in "$SRC_DIR"/*.yaml; do
	# fs.go-faster.org_fsclusters.yaml -> fsclusters.fs.go-faster.org.yaml
	base="$(basename "$src" .yaml)"
	group="${base%%_*}"
	plural="${base#*_}"
	dst="$DST_DIR/$plural.$group.yaml"

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
