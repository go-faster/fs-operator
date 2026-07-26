#!/usr/bin/env bash
#
# The operator's watch scope and its RBAC scope have to agree.
#
# They did not, and the failure had the worst possible shape: rbac.namespaced
# rendered a namespaced Role while the manager kept its cluster-wide cache, so
# every List and Watch was refused. The pod started, passed its health checks
# and reported Ready for as long as you left it there, reconciling nothing and
# logging "forbidden" every couple of seconds.
#
# Nothing caught it because nothing rendered the chart with that value set.
# This does. It asserts what the templates produce, which is where the two
# scopes are tied together; it does not install anything, so it proves the
# wiring rather than the behaviour.

set -euo pipefail

CHART="${CHART:-dist/chart}"
HELM="${HELM:-helm}"
failed=0

fail() {
	echo "FAIL: $*" >&2
	failed=1
}

render() {
	"$HELM" template scope-test "$CHART" "$@" 2>&1
}

# 1. The default watches everything, and says so by saying nothing: an empty
#    --watch-namespaces would mean "no namespaces", not "all of them".
out=$(render)
if grep -q -- "--watch-namespaces" <<<"$out"; then
	fail "the default install restricts the watch scope; it should watch every namespace"
fi

if ! grep -q "^kind: ClusterRole$" <<<"$out"; then
	fail "the default install has no ClusterRole"
fi

# 2. Namespaced RBAC narrows the watch scope to match, without being asked.
out=$(render --namespace fs-scope --set rbac.namespaced=true)
if ! grep -q -- "--watch-namespaces=fs-scope" <<<"$out"; then
	fail "rbac.namespaced did not narrow the watch scope to the release namespace"
fi

if ! grep -q "^kind: Role$" <<<"$out"; then
	fail "rbac.namespaced did not produce a namespaced Role"
fi

# 3. An explicit list is passed through, in order, with cluster-wide RBAC.
out=$(render --set 'watchNamespaces={team-a,team-b}')
if ! grep -q -- "--watch-namespaces=team-a,team-b" <<<"$out"; then
	fail "an explicit watchNamespaces list did not reach the manager"
fi

# 4. The combination that cannot work is refused while rendering, not at run
#    time: namespaced RBAC grants the release namespace only, so watching
#    another one is the original bug wearing a different hat.
if render --namespace fs-scope --set rbac.namespaced=true \
	--set 'watchNamespaces={team-a}' >/dev/null 2>&1; then
	fail "rbac.namespaced with a namespace outside the release namespace rendered; it should be refused"
fi

# 5. The same combination naming only the release namespace is consistent, so
#    it has to keep working — the guard must reject a mismatch, not the value.
out=$(render --namespace fs-scope --set rbac.namespaced=true \
	--set 'watchNamespaces={fs-scope}')
if ! grep -q -- "--watch-namespaces=fs-scope" <<<"$out"; then
	fail "rbac.namespaced with the release namespace listed explicitly was refused"
fi

if [ "$failed" -eq 0 ]; then
	echo "watch scope and RBAC scope agree in every combination"
fi

exit "$failed"
