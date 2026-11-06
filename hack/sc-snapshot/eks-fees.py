#!/usr/bin/env python3
"""Add the EKS Auto Mode management fees to a Spare Cores test snapshot.

The Keeper part of the snapshot is fetched by sc-keeper-go's cmd/sc-snapshot
(hack/sc-snapshot/queries.yaml); this step adds what only this repository
needs: the fees of the aws test region from the AWS public price list (the
AmazonEKS offer file, products with eksproducttype=AutoMode) as
`aws/eks-automode-fees.json.gz`, an {instance type: hourly USD fee} map listed
under `eks_automode_fees` in manifest.json. The Auto Mode adapter uses it as
its eligibility list; the replay server (internal/testsnapshot) answers price
list requests from it.

Run by the "Refresh Spare Cores snapshot" workflow after the fetcher:

    go run github.com/SpareCores/sc-keeper-go/cmd/sc-snapshot \\
        -queries hack/sc-snapshot/queries.yaml -out test/data/sc-snapshot
    python3 hack/sc-snapshot/eks-fees.py --out test/data/sc-snapshot

The manifest is re-written the way the fetcher writes it (sorted keys, indent
2), so only the fee entry differs. Only the Python standard library is used.
"""

import argparse
import gzip
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request

PRICE_LIST_URL = os.environ.get("AWS_PRICE_LIST_URL", "https://pricing.us-east-1.amazonaws.com")


def get_url(url: str, retries: int = 5):
    req = urllib.request.Request(url, headers={"User-Agent": "sc-karpenter-nodepool-snapshot"})
    for attempt in range(retries):
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            if exc.code in (429, 500, 502, 503, 504) and attempt < retries - 1:
                time.sleep(2**attempt * 5)
                continue
            raise SystemExit(f"GET {url} failed: HTTP {exc.code}: {exc.read()[:500]!r}")
        except urllib.error.URLError as exc:
            if attempt < retries - 1:
                time.sleep(2**attempt * 5)
                continue
            raise SystemExit(f"GET {url} failed: {exc}")


def automode_fees(offer: dict) -> dict[str, float]:
    """Extract {instance type: hourly USD fee} of the Auto Mode products of an
    AmazonEKS offer file (the same rules as the controller's parser)."""
    fees = {}
    terms = offer.get("terms", {}).get("OnDemand", {})
    for sku, product in offer.get("products", {}).items():
        attrs = product.get("attributes", {})
        if attrs.get("eksproducttype") != "AutoMode":
            continue
        instance_type = attrs.get("instancetype")
        dims = [d for term in terms.get(sku, {}).values() for d in term.get("priceDimensions", {}).values()]
        if not instance_type or len(dims) != 1 or dims[0].get("unit", "").lower() not in ("hours", "hrs"):
            raise SystemExit(f"unexpected Auto Mode product {sku} in the price list: {product} {dims}")
        fees[instance_type] = float(dims[0]["pricePerUnit"]["USD"])
    if not fees:
        raise SystemExit("the price list has no Auto Mode product")
    return fees


def write(out: str, name: str, data) -> dict:
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


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default="test/data/sc-snapshot", help="snapshot directory written by the fetcher")
    args = parser.parse_args()

    manifest_path = os.path.join(args.out, "manifest.json")
    with open(manifest_path) as f:
        manifest = json.load(f)
    aws_region = manifest["vendors"]["aws"]["region"]
    offer_url = f"{PRICE_LIST_URL}/offers/v1.0/aws/AmazonEKS/current/{aws_region}/index.json"
    offer = get_url(offer_url)
    fees = automode_fees(offer)
    manifest["eks_automode_fees"] = {
        "region": aws_region,
        "url": offer_url,
        "publication_date": offer.get("publicationDate"),
        "version": offer.get("version"),
        **write(args.out, "aws/eks-automode-fees", fees),
        "rows": len(fees),
    }
    with open(manifest_path, "w") as f:
        json.dump(manifest, f, indent=2, sort_keys=True)
        f.write("\n")
    print(f"{len(fees)} EKS Auto Mode fees of {aws_region} -> {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
