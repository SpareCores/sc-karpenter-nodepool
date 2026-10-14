#!/usr/bin/env python3
"""Turns a capture of record.py into asciinema v2 casts.

Commands are typed character by character (a seeded, human-like rhythm),
manifests are pasted at once, and the captured output is replayed with its
real timing, long waits time-lapsed. Chapters become markers. Writes:

  <out>/demo.cast            the demo
  <out>/demo-annotated.cast  the same, with the notes as comments (console)
  <out>/annotations.json     the notes on the timeline of demo.cast (web page)
  <out>/capture.json         the capture, redacted: compose again from it
                             without a cluster (uncompose.py rebuilds it from
                             demo.cast if it is lost)

    compose.py steps.yaml capture.json <out>

REDACT (environment, comma-separated) lists strings replaced by "<redacted>";
the output is checked for anything that looks like an AWS account.
"""

import json
import os
import random
import re
import sys
import textwrap

import yaml

PROMPT = "\x1b[1;32m$\x1b[0m "
NOTE_STYLE = "\x1b[2;3;36m"  # dim italic cyan
RESET = "\x1b[0m"
CLEAR = "\x1b[2J\x1b[3J\x1b[H"
CHAPTER_HOLD = 3.0  # seconds on a chapter's last output before the screen clears


class Cast:
    def __init__(self, seed: int):
        self.t = 0.0
        self.events: list[list] = []
        self.rng = random.Random(seed)

    def out(self, text: str, after: float = 0.0) -> None:
        self.t += after
        if text:
            self.events.append([round(self.t, 3), "o", text])

    def marker(self, label: str) -> None:
        self.events.append([round(self.t, 3), "m", label])

    def type(self, text: str) -> None:
        """Types text like a person: uneven keystrokes, short pauses at word ends."""
        for ch in text:
            delay = self.rng.uniform(0.035, 0.095)
            if ch == " ":
                delay += self.rng.uniform(0.0, 0.08)
            if self.rng.random() < 0.03:
                delay += self.rng.uniform(0.15, 0.4)  # a moment of thought
            self.out(ch, delay)


def wrap_command(cmd: str, width: int) -> list[str]:
    """Splits a long command at spaces into continuation lines (`\\`)."""
    lines, cur = [], ""
    for word in cmd.split(" "):
        if cur and len(cur) + 1 + len(word) > width - 6:
            lines.append(cur)
            cur = word
        else:
            cur = f"{cur} {word}" if cur else word
    lines.append(cur)
    return lines


def reading_time(note: str | None) -> float:
    return 0.0 if not note else min(7.0, max(1.5, len(note) * 0.035))


def compose(spec: dict, capture: dict, annotated: bool, redact: list[str]) -> tuple[dict, list, list]:
    cols, rows = spec["terminal"]["cols"], spec["terminal"]["rows"]
    by_index = {s["index"]: s for s in capture["steps"]}
    cast = Cast(seed=7)
    notes = []
    chapter = None
    for i, step in enumerate(spec["steps"]):
        if step.get("hidden"):
            continue
        cap = by_index[i]
        opens_chapter = "chapter" in step
        if opens_chapter:
            # Linger on the last output of the chapter, then clear the screen
            # and start the next command at once.
            chapter = step["chapter"]
            cast.out(CLEAR, CHAPTER_HOLD if cast.t else 0.0)
            cast.marker(chapter)
        note = step.get("note")
        if note:
            if notes:
                notes[-1]["end"] = round(cast.t, 3)
            notes.append({"start": round(cast.t, 3), "end": None, "chapter": chapter, "text": " ".join(note.split())})
            if annotated:
                for line in textwrap.wrap(" ".join(note.split()), cols - 2):
                    cast.out(f"{NOTE_STYLE}# {line}{RESET}\r\n", 0.05)
        if opens_chapter:
            cast.out(PROMPT, 0.1)
            cast.t += 0.3
        else:
            cast.out(PROMPT, 0.4)
            cast.t += reading_time(note) if not annotated else reading_time(note) + 1.0
        lines = wrap_command(" ".join(step["cmd"].split()), cols)
        for n, line in enumerate(lines):
            if n:
                cast.out("\r\n> ", 0.05)
            cast.type(line + (" \\" if n < len(lines) - 1 else ""))
        cast.out("\r\n", cast.rng.uniform(0.25, 0.5))
        if "paste" in step:
            cast.t += 0.6  # reaching for the clipboard
            for line in step["paste"].rstrip("\n").split("\n"):
                cast.out(f"> {line}\r\n", 0.012)
        # The output, with the real timing; long waits time-lapsed.
        cap_gap = float(step.get("maxGap", 1.4 if step.get("until") else 1.0))
        last = 0.0
        for offset, text in cap["chunks"]:
            gap = offset - last
            last = offset
            for secret in redact:
                text = text.replace(secret, "<redacted>")
            cast.out(text, min(gap, cap_gap) if gap > 0.05 else gap)
        cast.t += cast.rng.uniform(0.9, 1.4)
    cast.out("", 2.0)
    cast.out(PROMPT)
    if notes:
        notes[-1]["end"] = round(cast.t, 3)
    header = {
        "version": 2,
        "width": cols,
        "height": rows,
        "timestamp": capture["recordedAt"],
        "title": "sc-karpenter-nodepool on EKS Auto Mode",
        "env": {"TERM": "xterm-256color", "SHELL": "/bin/bash"},
    }
    return header, cast.events, notes


def check(text: str) -> None:
    for pattern in (r"(?<![0-9])[0-9]{12}(?![0-9])", r"arn:aws:[a-z0-9-]+:[a-z0-9-]*:[0-9]", r"attila"):
        m = re.search(pattern, text)
        if m:
            sys.exit(f"refusing to write: {pattern!r} matches {text[max(0, m.start() - 40):m.end() + 40]!r}")


def main() -> None:
    steps_file, capture_file, out = sys.argv[1:4]
    spec = yaml.safe_load(open(steps_file))
    capture = json.load(open(capture_file))
    redact = [s for s in os.environ.get("REDACT", "").split(",") if s]
    os.makedirs(out, exist_ok=True)
    kept = json.loads(json.dumps(capture))
    for st in kept["steps"]:
        for chunk in st["chunks"]:
            for secret in redact:
                chunk[1] = chunk[1].replace(secret, "<redacted>")
    body = json.dumps(kept)
    check(body)
    if os.path.abspath(capture_file) != os.path.abspath(os.path.join(out, "capture.json")):
        open(os.path.join(out, "capture.json"), "w").write(body)
    for name, annotated in (("demo.cast", False), ("demo-annotated.cast", True)):
        header, events, notes = compose(spec, capture, annotated, redact)
        body = json.dumps(header) + "\n" + "".join(json.dumps(e, ensure_ascii=False) + "\n" for e in events)
        check(body)
        open(os.path.join(out, name), "w").write(body)
        if not annotated:
            chapters = [{"start": e[0], "label": e[2]} for e in events if e[1] == "m"]
            json.dump(
                {"duration": events[-1][0], "chapters": chapters, "notes": notes},
                open(os.path.join(out, "annotations.json"), "w"),
                indent=1,
            )
        print(f"{name}: {events[-1][0]:.0f}s, {sum(1 for e in events if e[1] == 'm')} chapters")


if __name__ == "__main__":
    main()
