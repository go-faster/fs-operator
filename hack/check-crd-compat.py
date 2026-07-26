#!/usr/bin/env python3
"""Fail on a CRD schema change that would break objects already stored.

The CRDs are the operator's API. Once a user has applied an FSCluster, the
schema that validated it is a promise: a later release that drops a field
prunes it out of stored objects, and one that tightens a rule leaves objects
the API server will refuse on the next write — a cluster that reconciles fine
until someone edits an unrelated field.

This compares the generated CRDs against the last released tag's and reports
the changes that break that promise. It is deliberately one-directional:
additive changes (new optional fields, widened bounds, new enum values) are
what a v1alpha1 API is for and pass silently.

Usage:
  hack/check-crd-compat.py            # against the latest v* tag
  hack/check-crd-compat.py v0.4.0     # against a specific ref
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the script says how to fix itself.
    sys.exit("PyYAML is required: pip install pyyaml (or apt install python3-yaml)")

CRD_DIR = Path("config/crd/bases")

# ALLOW_FILE lists findings a human has reviewed and accepted. The checker
# compares schemas and cannot see the objects a cluster holds, so a rule that
# every stored object already satisfies looks identical to one that breaks
# them all — the difference is a judgement, and it gets written down there.
ALLOW_FILE = Path("hack/crd-compat-allow.txt")

# The one default that is *meant* to move. Every operator release pins the fs
# version it was validated against, and a cluster that omits spec.image.tag
# inherits it — that is the point of the default, and hack/set-fs-version.sh
# owns it. Every other default change is caught: silently changing what a field
# means for objects that never set it is the bug this check exists to find.
EXPECTED_DEFAULT_CHANGES = {"FSCluster/v1alpha1.spec.image.tag"}


def run(*args: str) -> str:
    """Run a git command, returning stdout."""
    return subprocess.run(
        args, capture_output=True, text=True, check=True
    ).stdout


def latest_tag() -> str | None:
    """The most recent release tag, or None when the repo has none."""
    try:
        tags = run("git", "tag", "--list", "v*", "--sort=-v:refname").split()
    except subprocess.CalledProcessError:
        return None

    return tags[0] if tags else None


def crd_at(ref: str, path: Path) -> dict | None:
    """The CRD as it was at ref, or None when it did not exist yet."""
    try:
        return yaml.safe_load(run("git", "show", f"{ref}:{path}"))
    except subprocess.CalledProcessError:
        return None


def versions(crd: dict) -> dict[str, dict]:
    """The CRD's versions by name."""
    return {v["name"]: v for v in crd.get("spec", {}).get("versions", [])}


def compare_schema(path: str, old: dict, new: dict, out: list[str]) -> None:
    """Walk two schemas together, reporting what a stored object would lose.

    `path` is the dotted field path, used only for the message.
    """
    old_type, new_type = old.get("type"), new.get("type")
    if old_type != new_type:
        out.append(f"{path}: type {old_type!r} -> {new_type!r}")
        # Everything below is about to disagree; the type change is the story.
        return

    # A field that is no longer described is pruned out of stored objects.
    old_props = old.get("properties", {})
    new_props = new.get("properties", {})

    for name in sorted(set(old_props) - set(new_props)):
        out.append(f"{path}.{name}: removed (stored objects lose this field)")

    # A newly required field makes every stored object without it invalid.
    old_required = set(old.get("required", []))
    new_required = set(new.get("required", []))

    for name in sorted(new_required - old_required):
        # Newly added *and* required is fine only if it has a default, which
        # the API server fills in on read.
        if name not in old_props and "default" in new_props.get(name, {}):
            continue

        out.append(f"{path}.{name}: newly required")

    # A default the user never wrote silently changes what their object means.
    if (
        old.get("default") != new.get("default")
        and "default" in old
        and path not in EXPECTED_DEFAULT_CHANGES
    ):
        out.append(f"{path}: default {old['default']!r} -> {new.get('default')!r}")

    # Dropping an enum value invalidates objects already holding it.
    old_enum, new_enum = old.get("enum"), new.get("enum")
    if old_enum and new_enum:
        for value in [v for v in old_enum if v not in new_enum]:
            out.append(f"{path}: enum value {value!r} no longer accepted")
    elif old_enum is None and new_enum is not None:
        out.append(f"{path}: newly constrained to {new_enum!r}")

    # Tightened bounds reject values that were legal when they were written.
    for key, tightened in (
        ("minimum", lambda o, n: n > o),
        ("maximum", lambda o, n: n < o),
        ("minLength", lambda o, n: n > o),
        ("maxLength", lambda o, n: n < o),
        ("minItems", lambda o, n: n > o),
        ("maxItems", lambda o, n: n < o),
    ):
        old_bound, new_bound = old.get(key), new.get(key)

        if new_bound is None:
            continue

        if old_bound is None or tightened(old_bound, new_bound):
            out.append(f"{path}: {key} {old_bound} -> {new_bound}")

    if old.get("pattern") != new.get("pattern") and new.get("pattern") is not None:
        out.append(f"{path}: pattern {old.get('pattern')!r} -> {new['pattern']!r}")

    # A new CEL rule can reject an object that was valid when stored.
    old_rules = {r.get("rule") for r in old.get("x-kubernetes-validations", [])}
    new_rules = {r.get("rule") for r in new.get("x-kubernetes-validations", [])}

    for rule in sorted(new_rules - old_rules):
        out.append(f"{path}: new validation rule {rule!r}")

    # Recurse into what both still describe.
    for name in sorted(set(old_props) & set(new_props)):
        compare_schema(f"{path}.{name}", old_props[name], new_props[name], out)

    if "items" in old and "items" in new:
        compare_schema(f"{path}[]", old["items"], new["items"], out)


def check(ref: str) -> list[str]:
    """Every incompatibility between ref's CRDs and the generated ones."""
    findings: list[str] = []

    for path in sorted(CRD_DIR.glob("*.yaml")):
        before = crd_at(ref, path)
        if before is None:
            # A CRD that did not exist at ref cannot break anything stored.
            continue

        after = yaml.safe_load(path.read_text())
        kind = after["spec"]["names"]["kind"]

        old_versions, new_versions = versions(before), versions(after)

        for name in sorted(set(old_versions) - set(new_versions)):
            findings.append(f"{kind}: version {name} removed")

        for name in sorted(set(old_versions) & set(new_versions)):
            old_version, new_version = old_versions[name], new_versions[name]

            if old_version.get("served") and not new_version.get("served"):
                findings.append(f"{kind}/{name}: no longer served")

            compare_schema(
                f"{kind}/{name}",
                old_version.get("schema", {}).get("openAPIV3Schema", {}),
                new_version.get("schema", {}).get("openAPIV3Schema", {}),
                findings,
            )

    return findings


def allowed() -> set[str]:
    """The findings hack/crd-compat-allow.txt accepts."""
    if not ALLOW_FILE.exists():
        return set()

    return {
        line.strip()
        for line in ALLOW_FILE.read_text().splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    }


def main() -> int:
    ref = sys.argv[1] if len(sys.argv) > 1 else latest_tag()

    if not ref:
        print("no release tag to compare against; skipping the CRD compatibility check")
        return 0

    accepted = allowed()
    findings = [f for f in check(ref) if f not in accepted]

    if not findings:
        if accepted:
            print(f"CRDs are compatible with {ref} ({len(accepted)} acknowledged change(s))")
        else:
            print(f"CRDs are compatible with {ref}")

        return 0

    print(f"CRD changes incompatible with objects stored under {ref}:\n", file=sys.stderr)

    for finding in findings:
        print(f"  {finding}", file=sys.stderr)

    print(
        f"\nEach of these changes the meaning of an object a user has already applied.\n"
        f"If a change is genuinely safe — a rule every stored object already\n"
        f"satisfies, say — copy the line into {ALLOW_FILE} with a comment saying\n"
        f"why. If it is not, it needs a new API version.\n",
        file=sys.stderr,
    )

    return 1


if __name__ == "__main__":
    sys.exit(main())
