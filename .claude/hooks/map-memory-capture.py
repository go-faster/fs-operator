#!/usr/bin/env python3
# MAP-MANAGED: {"generated_by":"mapify-cli","mapify_version":"3.23.0","template_hash":"40c3bf1cb5d88e03dd9326546c8f0c7346afd61cb99314df860c136d5eb4850b","installed_at":"2026-07-24T00:36:20Z"}
# map:start
"""Append per-turn scratch WAL record (cross-session memory). (REQUIRE_GUARD: MAP_INVOKED_BY)."""
import json
import os
import sys
from pathlib import Path

PROJECT_DIR = Path(os.environ.get("CLAUDE_PROJECT_DIR", os.getcwd()))


def _silent() -> None:
    sys.stdout.write("{}")
    sys.exit(0)


def main() -> None:
    if os.environ.get("MAP_INVOKED_BY"):   # FIRST statement — recursion guard
        sys.exit(0)
    try:
        input_data = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        _silent()
        return
    # src/ first (dogfood), falls back to installed mapify_cli; no-op if absent.
    sys.path.insert(0, str(PROJECT_DIR / "src"))
    try:
        from mapify_cli.memory.capture import append_turn
    except ImportError:
        _silent()
        return
    try:
        append_turn(input_data, PROJECT_DIR)
    except Exception:   # noqa: BLE001 — hooks must never block
        pass
    _silent()


if __name__ == "__main__":
    main()
# map:end
