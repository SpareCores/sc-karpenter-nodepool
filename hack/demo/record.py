#!/usr/bin/env python3
"""Runs the demo steps for real and captures what the terminal shows.

Every step of steps.yaml runs in bash inside a pseudo-terminal of the demo's
size, so tools format their output exactly as they would for a viewer. The
capture (output chunks with their time offsets) goes to a JSON file that
compose.py turns into casts. Needs KUBECONFIG pointing at the demo cluster.

    record.py steps.yaml capture.json
"""

import json
import os
import pty
import select
import signal
import struct
import subprocess
import sys
import termios
import fcntl
import time

import yaml


def run_in_pty(cmd: str, cols: int, rows: int, until: str | None, settle: float, max_wait: float):
    """Runs cmd in a pty; returns [(offset_seconds, text)] and the exit status."""
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update({"COLUMNS": str(cols), "LINES": str(rows), "TERM": "xterm-256color"})
        os.execvp("bash", ["bash", "--noprofile", "--norc", "-c", cmd])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    start = time.monotonic()
    chunks: list[tuple[float, str]] = []
    satisfied_at = None
    next_check = start + 3
    interrupted = False
    while True:
        r, _, _ = select.select([fd], [], [], 0.25)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            chunks.append((time.monotonic() - start, data.decode("utf-8", "replace")))
        now = time.monotonic()
        if until and not interrupted:
            if satisfied_at is None and now >= next_check:
                next_check = now + 3
                if subprocess.run(["bash", "-c", until], capture_output=True).returncode == 0:
                    satisfied_at = now
            if satisfied_at is not None and now - satisfied_at >= settle:
                os.write(fd, b"\x03")  # ^C through the tty, as a person would
                interrupted = True
        if now - start > max_wait and not interrupted:
            os.write(fd, b"\x03")
            interrupted = True
            print(f"  ! timed out after {max_wait}s", file=sys.stderr)
    _, status = os.waitpid(pid, 0)
    os.close(fd)
    code = os.waitstatus_to_exitcode(status)
    if interrupted and satisfied_at is not None:
        code = 0  # a watch stopped by ^C once its condition held (kubectl -w exits 1)
    return chunks, code, time.monotonic() - start


def main() -> None:
    steps_file, out_file = sys.argv[1], sys.argv[2]
    spec = yaml.safe_load(open(steps_file))
    cols, rows = spec["terminal"]["cols"], spec["terminal"]["rows"]
    captured = []
    for i, step in enumerate(spec["steps"]):
        cmd = step["cmd"]
        if "paste" in step:
            cmd = f"{cmd}\n{step['paste']}"
        print(f"[{i}] {step['cmd'][:90]}", file=sys.stderr, flush=True)
        if step.get("hidden"):
            res = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True)
            if res.returncode != 0:
                sys.exit(f"hidden step failed: {res.stdout}{res.stderr}")
            continue
        chunks, code, elapsed = run_in_pty(
            cmd, cols, rows, step.get("until"), float(step.get("settle", 2)), float(step.get("maxWait", 900))
        )
        if code not in (0, -signal.SIGINT, 130) and not step.get("mayFail"):
            sys.exit(f"step {i} exited {code}:\n{''.join(c for _, c in chunks)}")
        captured.append({"index": i, "chunks": chunks, "exit": code, "elapsed": elapsed})
        print("".join(c for _, c in chunks)[-2000:], file=sys.stderr, flush=True)
    json.dump({"terminal": spec["terminal"], "recordedAt": int(time.time()), "steps": captured}, open(out_file, "w"))


if __name__ == "__main__":
    main()
