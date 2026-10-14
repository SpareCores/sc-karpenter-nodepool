#!/usr/bin/env python3
"""Recovers a capture from a cast made by compose.py.

compose.py writes every step as: [clear + marker at a chapter], [notes as
comments in the annotated cast], the prompt, the typed command ending with a
lone "\\r\\n", the pasted manifest lines ("> ..."), then the step's output
with its (time-lapsed) timing. This walks demo.cast (not the annotated one)
step by step and rebuilds the output chunks with their offsets, so the casts
can be composed again (another layout, other timing) without re-recording.

    uncompose.py steps.yaml demo.cast capture.json
"""

import json
import sys

import yaml

PROMPT = "\x1b[1;32m$\x1b[0m "
CLEAR = "\x1b[2J\x1b[3J\x1b[H"


def main() -> None:
    steps_file, cast_file, out_file = sys.argv[1:4]
    spec = yaml.safe_load(open(steps_file))
    lines = open(cast_file).read().splitlines()
    header = json.loads(lines[0])
    events = [json.loads(l) for l in lines[1:] if l.strip()]
    out = [e for e in events if e[1] == "o"]
    pos = 0
    captured = []
    for i, step in enumerate(spec["steps"]):
        if step.get("hidden"):
            continue
        while out[pos][2] != PROMPT:  # skip clear and annotation comments
            pos += 1
        pos += 1
        while out[pos][2] != "\r\n":  # the typed command
            pos += 1
        last = out[pos][0]
        pos += 1
        if "paste" in step:
            n = len(step["paste"].rstrip("\n").split("\n"))
            assert all(e[2].startswith("> ") for e in out[pos : pos + n]), f"step {i}: paste not found"
            last = out[pos + n - 1][0]
            pos += n
        chunks = []
        offset = 0.0
        while pos < len(out) and out[pos][2] not in (PROMPT, CLEAR):
            offset += out[pos][0] - last
            last = out[pos][0]
            chunks.append([round(offset, 6), out[pos][2]])
            pos += 1
        captured.append({"index": i, "chunks": chunks, "exit": 0, "elapsed": offset})
    json.dump({"terminal": spec["terminal"], "recordedAt": header["timestamp"], "steps": captured}, open(out_file, "w"))
    print(f"{len(captured)} steps, {sum(len(s['chunks']) for s in captured)} output chunks")


if __name__ == "__main__":
    main()
