#!/usr/bin/env python3
"""Publish a new static GTFS ZIP into the versioned handover store.

The operator workflow this tool implements:

  1. The candidate ZIP is inspected *in place* (never executed) and its static
     key is resolved: feed_info.txt's feed_version, or a sha256 fallback.
  2. The new ZIP and the GTFS-Realtime protos that were successfully built
     from it (if provided) are staged into a per-version directory using
     temp-file + rename, so readers never see partial files.
  3. versions.json is rewritten atomically with the new pair first and at
     most the two newest pairs retained.

If anything fails before the registry is rewritten, the previous pair - and
its static ZIP / protos - stay completely untouched and readable.

Only the Python standard library is used. Run with the isolated interpreter
when the candidate ZIP is untrusted input:

    python -I tools/publish_static.py --store /var/gtfs-store new_gtfs.zip
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import io
import json
import os
import shutil
import sys
import zipfile
from datetime import datetime, timezone
from pathlib import Path

REGISTRY_NAME = "versions.json"
REGISTRY_FORMAT_VERSION = 1
MAX_PAIRS = 2
HASH_PREFIX = "sha256:"
FEED_INFO_NAME = "feed_info.txt"
FEED_VERSION_COLUMN = "feed_version"


class PublishError(Exception):
    """Fatal, user-facing publication error."""


def hash_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def read_feed_version(zf: zipfile.ZipFile) -> str:
    """Return the first non-empty feed_version, or '' if unavailable."""
    try:
        raw = zf.read(FEED_INFO_NAME)
    except KeyError:
        return ""
    try:
        text = raw.decode("utf-8-sig", errors="replace")
        reader = csv.reader(io.StringIO(text))
        try:
            header = next(reader)
        except StopIteration:
            return ""
        if FEED_VERSION_COLUMN not in header:
            return ""
        col = header.index(FEED_VERSION_COLUMN)
        for row in reader:
            if col < len(row) and row[col].strip():
                return row[col].strip()
    except (csv.Error, UnicodeError):
        return ""
    return ""


def describe(candidate: Path) -> dict:
    if not candidate.is_file():
        raise PublishError(f"candidate ZIP not found: {candidate}")
    digest = hash_file(candidate)
    try:
        with zipfile.ZipFile(candidate) as zf:
            bad = zf.testzip()
            if bad is not None:
                raise PublishError(f"corrupt entry inside candidate ZIP: {bad}")
            feed_version = read_feed_version(zf)
    except zipfile.BadZipFile as e:
        raise PublishError(f"not a valid ZIP archive: {candidate}: {e}") from e

    key = feed_version or (HASH_PREFIX + digest)
    return {
        "key": key,
        "feed_version": feed_version,
        "sha256": digest,
    }


def sanitize_name(key: str) -> str:
    out = []
    for ch in key:
        if ch.isalnum() or ch in ".-_":
            out.append(ch)
        else:
            out.append("_")
    return "".join(out) or "unknown"


def atomic_copy(src: Path, dst: Path) -> None:
    dst.parent.mkdir(parents=True, exist_ok=True)
    tmp = dst.with_name(".tmp-" + dst.name + ".part")
    with src.open("rb") as fin, tmp.open("wb") as fout:
        for chunk in iter(lambda: fin.read(1024 * 1024), b""):
            fout.write(chunk)
        fout.flush()
        os.fsync(fout.fileno())
    os.replace(tmp, dst)


def atomic_write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(".tmp-" + path.name + ".part")
    with tmp.open("w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2, ensure_ascii=False)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def load_registry(store: Path) -> dict:
    reg_path = store / REGISTRY_NAME
    if not reg_path.exists():
        return {"version": REGISTRY_FORMAT_VERSION, "pairs": []}
    try:
        doc = json.loads(reg_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        raise PublishError(f"cannot read registry {reg_path}: {e}") from e
    if doc.get("version") != REGISTRY_FORMAT_VERSION:
        raise PublishError(
            f"unsupported registry format {doc.get('version')!r}; "
            f"want {REGISTRY_FORMAT_VERSION}"
        )
    return doc


def stage_proto(flag_value: str | None, store: Path, pair_dir: str, name: str) -> str:
    if not flag_value:
        return ""
    src = Path(flag_value)
    if not src.is_file():
        raise PublishError(f"proto file not found: {src}")
    rel = f"{pair_dir}/{name}"
    atomic_copy(src, store / rel)
    return rel


def publish(
    store: Path,
    candidate: Path,
    alerts_proto: str | None,
    positions_proto: str | None,
) -> dict:
    store.mkdir(parents=True, exist_ok=True)
    registry = load_registry(store)

    static = describe(candidate)
    for pair in registry["pairs"]:
        if pair["static"]["key"] == static["key"]:
            print(f"static key {static['key']!r} already published; nothing to do")
            return pair

    pair_dir = f"versions/{sanitize_name(static['key'])}"

    # Guard against sanitized-name collisions with retained pairs: two
    # distinct keys must never share a per-pair directory.
    for old in registry["pairs"]:
        old_dir = f"versions/{sanitize_name(old['static']['key'])}"
        if old_dir == pair_dir and old["static"]["key"] != static["key"]:
            raise PublishError(
                f"static key {static['key']!r} sanitizes to {pair_dir!r}, "
                f"already used by key {old['static']['key']!r}"
            )

    pair = {
        "static": {
            **static,
            "path": f"{pair_dir}/static.zip",
            "published_at": datetime.now(timezone.utc)
            .isoformat(timespec="seconds")
            .replace("+00:00", "Z"),
        },
    }

    # Stage every artifact first. Any failure here only removes this pair's
    # own staging directory - the registry and previous pairs are untouched.
    staged: list[str] = []
    try:
        atomic_copy(candidate, store / pair["static"]["path"])
        staged.append(pair["static"]["path"])

        alerts_rel = stage_proto(alerts_proto, store, pair_dir, "alerts.pb")
        if alerts_rel:
            pair["alerts"] = alerts_rel
        positions_rel = stage_proto(positions_proto, store, pair_dir, "positions.pb")
        if positions_rel:
            pair["positions"] = positions_rel

        new_pairs = [pair] + registry["pairs"][: MAX_PAIRS - 1]
        atomic_write_json(store / REGISTRY_NAME,
                          {"version": REGISTRY_FORMAT_VERSION, "pairs": new_pairs})
    except BaseException:
        # Roll back anything staged for the failed pair.
        shutil.rmtree(store / pair_dir, ignore_errors=True)
        raise

    # Evict directories of pairs that dropped out of the retained window.
    retained_dirs = {
        f"versions/{sanitize_name(p['static']['key'])}" for p in new_pairs
    }
    for old in registry["pairs"][MAX_PAIRS - 1:]:
        old_dir = f"versions/{sanitize_name(old['static']['key'])}"
        if old_dir not in retained_dirs:
            shutil.rmtree(store / old_dir, ignore_errors=True)

    print(f"published static key {static['key']!r}")
    print(f"  sha256: {static['sha256']}")
    print(f"  registry now holds {len(new_pairs)} pair(s)")
    return pair


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", type=Path, help="new static GTFS ZIP to publish")
    parser.add_argument(
        "--store",
        type=Path,
        required=True,
        help="root of the versioned handover store (holds versions.json)",
    )
    parser.add_argument(
        "--alerts-proto",
        help="alerts GTFS-RT proto successfully built from the candidate ZIP",
    )
    parser.add_argument(
        "--positions-proto",
        help="positions GTFS-RT proto successfully built from the candidate ZIP",
    )
    args = parser.parse_args(argv)

    try:
        publish(args.store, args.candidate, args.alerts_proto, args.positions_proto)
    except PublishError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
