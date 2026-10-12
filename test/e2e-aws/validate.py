#!/usr/bin/env python3
"""Validate a JSON document against a JSON schema (draft 7), offline.

    validate.py <schema.json> <document.json> [--expect-invalid]

Exit status 0 if the document is valid (or, with --expect-invalid, if it is
not: used to show that the validation is not vacuous). Errors are printed with
their path in the document.
"""
import json
import sys

import jsonschema


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    expect_invalid = "--expect-invalid" in sys.argv
    if len(args) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    with open(args[0], encoding="utf-8") as f:
        schema = json.load(f)
    with open(args[1], encoding="utf-8") as f:
        document = json.load(f)
    validator = jsonschema.Draft7Validator(schema)
    errors = sorted(validator.iter_errors(document), key=lambda e: list(e.absolute_path))
    for e in errors:
        where = "/".join(str(p) for p in e.absolute_path) or "(root)"
        print(f"  {where}: {e.message[:300]}", file=sys.stderr)
    if expect_invalid:
        if errors:
            return 0
        print("  the document was expected to be invalid and is not", file=sys.stderr)
        return 1
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
