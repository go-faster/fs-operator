#!/usr/bin/env bash
# Pin the go-faster/fs release this operator is validated against.
#
# The version is a literal in several places — a Go constant, a kubebuilder
# default marker, the examples, the e2e suite, the docs — because none of them
# can read a variable: controller-gen needs a literal to put in the CRD schema,
# and an example that is not copy-pasteable is not an example. So the pin lives
# in one place conceptually and is propagated from here.
#
# Usage:
#   hack/set-fs-version.sh v0.6.0   # rewrite the pin and regenerate
#   hack/set-fs-version.sh --check  # fail if the copies disagree (CI)
set -euo pipefail

cd "$(dirname "$0")/.."

# canonical is where the pin is read from: the constant the controller uses.
canonical="api/v1alpha1/defaults.go"

# current reads the pinned version out of the canonical source.
current() {
	sed -n 's/^\tDefaultImageTag = "\(.*\)"$/\1/p' "${canonical}"
}

# files lists every file that spells the version out, and the pattern that
# matches the line it appears on. A new occurrence belongs here — `--check`
# then keeps it from drifting.
targets=(
	"api/v1alpha1/defaults.go:	DefaultImageTag = \"%s\""
	"api/v1alpha1/fscluster_types.go:	// +kubebuilder:default=\"%s\""
	"test/e2e/e2e_suite_test.go:const fsImage = \"ghcr.io/go-faster/fs:%s\""
	"examples/02-zonal-racks.yaml:    tag: %s"
	"examples/03-multi-disk.yaml:    tag: %s"
	"SPEC.md:    tag: %s"
	"SPEC.md:    # against (currently %s). Always a pinned version, never a floating"
)

version="${1:-}"
if [[ -z "${version}" ]]; then
	echo "usage: $0 <vX.Y.Z|--check>" >&2
	exit 2
fi

pinned="$(current)"
if [[ -z "${pinned}" ]]; then
	echo "cannot read DefaultImageTag from ${canonical}" >&2
	exit 1
fi

if [[ "${version}" == "--check" ]]; then
	status=0

	for target in "${targets[@]}"; do
		file="${target%%:*}"
		template="${target#*:}"
		# shellcheck disable=SC2059 # the template is ours, not user input.
		line="$(printf "${template}" "${pinned}")"

		if ! grep -qxF "${line}" "${file}"; then
			echo "${file}: does not pin ${pinned} (expected line: ${line})" >&2
			status=1
		fi
	done

	if [[ ${status} -eq 0 ]]; then
		echo "fs version ${pinned} is pinned consistently"
	fi

	exit ${status}
fi

if [[ ! "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "fs version must be a pinned release like v0.6.0, got ${version}" >&2
	exit 2
fi

if [[ "${version}" == "${pinned}" ]]; then
	echo "fs is already pinned to ${pinned}"
	exit 0
fi

for target in "${targets[@]}"; do
	file="${target%%:*}"
	template="${target#*:}"
	# shellcheck disable=SC2059 # the template is ours, not user input.
	from="$(printf "${template}" "${pinned}")"
	# shellcheck disable=SC2059
	to="$(printf "${template}" "${version}")"

	if ! grep -qxF "${from}" "${file}"; then
		echo "${file}: no line pinning ${pinned}; fix the target list in $0" >&2
		exit 1
	fi

	python3 - "${file}" "${from}" "${to}" <<-'EOF'
		import sys

		path, before, after = sys.argv[1], sys.argv[2], sys.argv[3]

		with open(path, encoding="utf-8") as f:
		    content = f.read()

		with open(path, "w", encoding="utf-8") as f:
		    f.write(content.replace(before + "\n", after + "\n"))
	EOF
done

echo "pinned fs ${pinned} -> ${version}; regenerating manifests and chart CRDs"

make manifests generate helm-sync-crds

cat <<EOF

fs is now pinned to ${version}. Before committing:
  - run 'make test' and the e2e suite against the new image;
  - read the upstream release notes for schema changes: a cluster whose
    binary implements a newer schema needs a migration, and a rollback past
    one is unsupported (SPEC §8.2).
EOF
