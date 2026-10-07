#!/usr/bin/env python3
"""Fetch a Spare Cores data snapshot from the Keeper API for the test lanes.

The snapshot is a fixed set of raw Keeper responses, so tests are deterministic
and offline:

- L1 replays them through a fake Keeper (recorded fixtures for the client and
  the selector engine);
- L3 builds the per-vendor KWOK instance-type catalogs from them.

Run manually (or via the "Refresh Spare Cores snapshot" workflow, which commits
the result). The output directory is replaced as a whole. Responses are stored
gzipped (a full AWS /servers response is ~4.5 MB of JSON, ~250 KB gzipped);
`manifest.json` lists every query with its parameters, file, row count and
SHA-256, plus the Keeper database hash the data was read from.

Only the Python standard library is used.
"""

import argparse
import gzip
import hashlib
import json
import os
import shutil
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

KEEPER_URL = os.environ.get("KEEPER_URL", "https://keeper.sparecores.net")

# One test region per vendor (the Kubernetes region value; mapped to Keeper's
# region_id via /table/region, as GCP and Hetzner region ids are numeric).
# `spot` marks vendors with spot prices worth snapshotting.
VENDORS = {
    "aws": {"region": "us-east-1", "spot": True},
    "azure": {"region": "westeurope", "spot": True},
    "gcp": {"region": "us-central1", "spot": True},
    "alicloud": {"region": "eu-central-1", "spot": True},
    "hcloud": {"region": "fsn1", "spot": False},
    "ovh": {"region": "GRA11", "spot": False},
    "upcloud": {"region": "de-fra1", "spot": False},
    "vultr": {"region": "ewr", "spot": False},
}

# Benchmarks the tests select and rank on: single- and multi-core CPU, a
# composite profile, and one lower-is-better metric. Configs are matched
# against /benchmark_configs by value, and the canonical string is sent.
BENCHMARKS = [
    ("geekbench:score", {"cores": "single"}),
    ("passmark:cpu_single_threaded_test", {}),
    ("stress_ng:bestn", {}),
    ("workload_profile:web", {}),
    ("membench:latency", {"scope": "RAM"}),
]

ALLOCATIONS = {"ondemand": "ONDEMAND_ONLY", "spot": "SPOT_ONLY"}


def get(path: str, params: dict | None = None, retries: int = 5):
    url = f"{KEEPER_URL}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"User-Agent": "sc-karpenter-nodepool-snapshot"})
    for attempt in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                remaining = resp.headers.get("x-ratelimit-remaining")
                body = resp.read()
                # stay well below the anonymous 300 credits/minute limit
                if remaining is not None and int(remaining) < 30:
                    time.sleep(30)
                return json.loads(body)
        except urllib.error.HTTPError as exc:
            if exc.code in (429, 500, 502, 503, 504) and attempt < retries - 1:
                time.sleep(2 ** attempt * 5)
                continue
            raise SystemExit(f"GET {url} failed: HTTP {exc.code}: {exc.read()[:500]!r}")
        except urllib.error.URLError as exc:
            if attempt < retries - 1:
                time.sleep(2 ** attempt * 5)
                continue
            raise SystemExit(f"GET {url} failed: {exc}")


def canonical_config(configs: list[dict], benchmark_id: str, config: dict) -> str:
    for entry in configs:
        if entry["benchmark_id"] == benchmark_id and json.loads(entry["config"]) == config:
            return entry["config"]
    raise SystemExit(f"benchmark {benchmark_id} {config} not in /benchmark_configs")


def region_id(regions: list[dict], vendor: str, k8s_region: str) -> str:
    for r in regions:
        if r["vendor_id"] != vendor:
            continue
        if k8s_region in (r["region_id"], r["api_reference"]) or k8s_region in (r.get("aliases") or []):
            return r["region_id"]
    raise SystemExit(f"region {k8s_region} of {vendor} not found in /table/region")


def write(out: str, name: str, data) -> dict:
    # Keeper's order among equally priced rows is not stable; sorting keeps
    # the files byte-identical when the data is (the controller ranks
    # client-side, so the response order carries no meaning for us).
    if isinstance(data, list):
        data = sorted(data, key=lambda row: json.dumps(row, sort_keys=True))
    raw = json.dumps(data, sort_keys=True, separators=(",", ":")).encode()
    path = os.path.join(out, name + ".json.gz")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    # mtime=0 keeps the gzip bytes identical for identical data
    with open(path, "wb") as f:
        f.write(gzip.compress(raw, compresslevel=9, mtime=0))
    return {
        "file": os.path.relpath(path, out),
        "rows": len(data) if isinstance(data, list) else None,
        "sha256": hashlib.sha256(raw).hexdigest(),
    }


def fetch(out: str) -> dict:
    health = get("/healthcheck")
    queries = []

    def record(name: str, path: str, params: dict | None = None):
        data = get(path, params)
        queries.append({"name": name, "path": path, "params": params or {}, **write(out, name, data)})
        return data

    regions = record("tables/region", "/table/region")
    record("tables/zone", "/table/zone")
    configs = record("benchmark_configs", "/benchmark_configs")

    for vendor, spec in VENDORS.items():
        rid = region_id(regions, vendor, spec["region"])
        base = {"vendor": vendor, "vendor_regions": f"{vendor}~{rid}", "limit": -1}
        for alloc, keeper_alloc in ALLOCATIONS.items():
            if alloc == "spot" and not spec["spot"]:
                continue
            record(f"{vendor}/servers-{alloc}", "/servers", {**base, "best_price_allocation": keeper_alloc})
        for benchmark_id, config in BENCHMARKS:
            params = {
                **base,
                "best_price_allocation": ALLOCATIONS["ondemand"],
                "benchmark_id": benchmark_id,
                "benchmark_config": canonical_config(configs, benchmark_id, config),
            }
            name = benchmark_id.replace(":", "_")
            record(f"{vendor}/benchmark-{name}", "/servers", params)

    if get("/healthcheck").get("database_hash") != health.get("database_hash"):
        raise SystemExit("Keeper data changed during the fetch, run again")

    return {
        "fetched_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "keeper_url": KEEPER_URL,
        "keeper": {
            "packages": health.get("packages"),
            "database_hash": health.get("database_hash"),
            "database_last_updated": datetime.fromtimestamp(
                health["database_last_updated"], timezone.utc
            ).isoformat(timespec="seconds"),
        },
        "vendors": VENDORS,
        "queries": queries,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default="test/data/sc-snapshot", help="output directory (replaced)")
    args = parser.parse_args()

    tmp = args.out.rstrip("/") + ".tmp"
    shutil.rmtree(tmp, ignore_errors=True)
    os.makedirs(tmp)
    manifest = fetch(tmp)
    with open(os.path.join(tmp, "manifest.json"), "w") as f:
        json.dump(manifest, f, indent=2, sort_keys=True)
        f.write("\n")
    shutil.rmtree(args.out, ignore_errors=True)
    os.replace(tmp, args.out)
    print(
        f"snapshot of Keeper database {manifest['keeper']['database_hash']} "
        f"({manifest['keeper']['database_last_updated']}): {len(manifest['queries'])} queries -> {args.out}",
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
