#!/usr/bin/env python3
"""FileSync smoke test — identity-checked, not just "process alive".

Runs the built binary in a throwaway config dir and verifies, in
order:
  1. `init` succeeds and prints a device ID.
  2. `serve` starts; within 15s status.json says state=="running"
     AND device_id equals the init device ID (identity!) AND a TCP
     connection to listen_addr succeeds.
  3. SIGTERM stops the daemon cleanly: exit code 0 and a final
     status.json state of "stopped".

Usage: python3 scripts/smoke-test.py [--binary] [path-to-filesync-binary]
Both `smoke-test.py /path/to/filesync` and
`smoke-test.py --binary /path/to/filesync` are accepted.
Exit 0 = PASS, exit 1 = FAIL with diagnosis on stderr.
"""

import json
import os
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time


def fail(msg, extra=""):
    print(f"SMOKE FAIL: {msg}", file=sys.stderr)
    if extra:
        print(extra, file=sys.stderr)
    sys.exit(1)


def read_status(cfg_dir):
    try:
        with open(os.path.join(cfg_dir, "status.json")) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return None


def main():
    args = sys.argv[1:]
    if args[:1] == ["--binary"]:
        args = args[1:]
    binary = os.path.abspath(args[0] if args else "filesync")
    if not os.path.isfile(binary):
        fail(f"binary not found: {binary}")

    tmp = tempfile.mkdtemp(prefix="filesync-smoke-")
    cfg_dir = os.path.join(tmp, "config")

    # 1. init, capture the device ID (the identity marker).
    proc = subprocess.run(
        [binary, "--config-dir", cfg_dir, "init"],
        capture_output=True, text=True, timeout=30,
    )
    if proc.returncode != 0:
        fail("init exited non-zero", proc.stdout + proc.stderr)
    m = re.search(r"device_id:\s+(\S+)", proc.stdout)
    if not m:
        fail("init output has no device_id", proc.stdout)
    device_id = m.group(1)
    print(f"init: device_id = {device_id}")

    # 2. serve in background.
    env = dict(os.environ, FILESYNC_DISABLE_MDNS="1")
    daemon = subprocess.Popen(
        [binary, "--config-dir", cfg_dir, "serve"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env,
    )
    try:
        deadline = time.time() + 15
        status = None
        while time.time() < deadline:
            if daemon.poll() is not None:
                out = daemon.stdout.read() if daemon.stdout else ""
                fail(f"daemon exited early with code {daemon.returncode}", out)
            status = read_status(cfg_dir)
            if (status and status.get("state") == "running"
                    and status.get("listen_addr")):
                break
            time.sleep(0.2)
        if not status or status.get("state") != "running" or not status.get("listen_addr"):
            fail("status.json never reached state=running with a listen_addr in 15s",
                 f"last status: {status}")
        if status.get("device_id") != device_id:
            fail(f"identity mismatch: status device_id={status.get('device_id')} "
                 f"!= init device_id={device_id}")
        listen = status.get("listen_addr", "")
        host, _, port = listen.rpartition(":")
        host = host.strip("[]")
        if host in ("", "::", "0.0.0.0"):
            # Wildcard bind: reachable via loopback.
            host = "127.0.0.1"
        try:
            with socket.create_connection((host, int(port)), timeout=5):
                pass
        except OSError as exc:
            fail(f"cannot TCP-connect to listen_addr {listen}: {exc}")
        print(f"serve: running, identity verified, listening on {listen}")

        # 3. SIGTERM -> clean exit 0 + final status stopped.
        daemon.send_signal(signal.SIGTERM)
        try:
            rc = daemon.wait(timeout=15)
        except subprocess.TimeoutExpired:
            daemon.kill()
            fail("daemon did not exit within 15s of SIGTERM")
        if rc != 0:
            out = daemon.stdout.read() if daemon.stdout else ""
            fail(f"daemon exit code {rc} after SIGTERM (want 0)", out)
        final = read_status(cfg_dir)
        if not final or final.get("state") != "stopped":
            fail(f"final status is not 'stopped': {final}")
        print("stop: clean exit 0, final status 'stopped'")
    finally:
        if daemon.poll() is None:
            daemon.kill()

    print("SMOKE PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
