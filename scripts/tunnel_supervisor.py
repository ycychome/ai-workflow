#!/usr/bin/env python3
"""Supervise the webhook allowlist proxy and its cloudflared tunnel as a pair."""

from __future__ import annotations

import argparse
import os
import shutil
import signal
import socket
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Optional


ROOT = Path(__file__).resolve().parent.parent
PROXY_SCRIPT = ROOT / "scripts" / "webhook_proxy.py"
PROXY_HOST = "127.0.0.1"
PROXY_PORT = 8082
PROXY_URL = f"http://{PROXY_HOST}:{PROXY_PORT}"
POLL_SECONDS = 1.0
_stopping = False


def on_signal(signum, _frame):
    global _stopping
    _stopping = True


def process_details(pid: int):
    """Return the untruncated command for a live PID, or None if it has exited."""
    try:
        result = subprocess.run(
            ["ps", "-ww", "-p", str(pid), "-o", "command="],
            check=False,
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    line = result.stdout.strip()
    if result.returncode != 0 or not line:
        return None
    return line


def command_is_proxy(details: str) -> bool:
    return "webhook_proxy.py" in details and ("python" in details.lower())


def command_is_cloudflared(details: str) -> bool:
    lowered = details.lower()
    return "cloudflared" in lowered and "tunnel" in lowered and PROXY_URL in details


def process_start_time(pid: int) -> Optional[str]:
    try:
        result = subprocess.run(
            ["ps", "-ww", "-p", str(pid), "-o", "lstart="],
            check=False,
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    value = result.stdout.strip()
    return value if result.returncode == 0 and value else None


def port_listener_pids():
    lsof = shutil.which("lsof")
    if not lsof:
        return None
    try:
        result = subprocess.run(
            [lsof, "-nP", f"-iTCP:{PROXY_PORT}", "-sTCP:LISTEN", "-t"],
            check=False,
            capture_output=True,
            text=True,
            timeout=2,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    return {int(value) for value in result.stdout.split() if value.isdigit()}


def port_is_free() -> bool:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        sock.bind((PROXY_HOST, PROXY_PORT))
        return True
    except OSError:
        return False
    finally:
        sock.close()


def proxy_accepts_connections() -> bool:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(0.5)
    try:
        sock.connect((PROXY_HOST, PROXY_PORT))
        return True
    except OSError:
        return False
    finally:
        sock.close()


@dataclass
class ManagedProcess:
    pid: int
    role: str
    owned: bool
    popen: Optional[subprocess.Popen] = None
    start_time: Optional[str] = None

    def alive(self) -> bool:
        if self.owned:
            return self.popen is not None and self.popen.poll() is None
        details = process_details(self.pid)
        if details is None or not self.start_time:
            return False
        if process_start_time(self.pid) != self.start_time:
            return False
        return command_is_proxy(details) if self.role == "proxy" else command_is_cloudflared(details)

    def terminate(self):
        if self.owned:
            if self.popen is None or self.popen.poll() is not None:
                return
            self.popen.terminate()
            try:
                self.popen.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.popen.kill()
                self.popen.wait(timeout=3)
            return

        if not self.alive():
            print(f"Not stopping {self.role} PID {self.pid}: adopted process identity no longer matches.", file=sys.stderr)
            return
        try:
            os.kill(self.pid, signal.SIGTERM)
        except ProcessLookupError:
            return
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and self.alive():
            time.sleep(0.1)
        if self.alive():
            # Revalidate identity immediately before the stronger signal.
            if self.alive():
                os.kill(self.pid, signal.SIGKILL)


def validate_pid(pid: int, role: str) -> ManagedProcess:
    if pid <= 1 or pid == os.getpid():
        raise ValueError(f"Invalid {role} PID: {pid}")
    details = process_details(pid)
    expected = command_is_proxy(details or "") if role == "proxy" else command_is_cloudflared(details or "")
    if not expected:
        raise ValueError(f"PID {pid} is not the expected {role} process; refusing to adopt it.")
    started = process_start_time(pid)
    if not started:
        raise ValueError(f"Could not verify the start time for {role} PID {pid}.")
    return ManagedProcess(pid=pid, role=role, owned=False, start_time=started)


def make_proxy() -> ManagedProcess:
    listeners = port_listener_pids()
    if not port_is_free():
        owner_text = f" (listener PID(s): {', '.join(map(str, sorted(listeners)))})" if listeners else ""
        raise RuntimeError(f"Cannot start webhook proxy: 127.0.0.1:{PROXY_PORT} is already occupied{owner_text}; refusing to stop an unrelated process.")
    proc = subprocess.Popen([sys.executable, "-u", str(PROXY_SCRIPT)], cwd=ROOT)
    managed = ManagedProcess(pid=proc.pid, role="proxy", owned=True, popen=proc)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"Webhook proxy exited during startup (status {proc.returncode}).")
        if proxy_accepts_connections():
            listeners = port_listener_pids()
            if listeners is not None and listeners != {proc.pid}:
                managed.terminate()
                raise RuntimeError(f"Port {PROXY_PORT} is served by an unexpected process; refusing to use or stop it.")
            return managed
        time.sleep(0.1)
    managed.terminate()
    raise RuntimeError(f"Webhook proxy did not open {PROXY_URL} within five seconds.")


def attach_proxy(pid: int) -> ManagedProcess:
    managed = validate_pid(pid, "proxy")
    if not proxy_accepts_connections():
        raise ValueError(f"Adopted webhook proxy PID {pid} is not accepting connections on {PROXY_URL}.")
    listeners = port_listener_pids()
    if listeners is not None and listeners != {pid}:
        raise ValueError(f"Adopted proxy PID {pid} does not own port {PROXY_PORT}.")
    return managed


def make_cloudflared() -> ManagedProcess:
    executable = shutil.which("cloudflared")
    if not executable:
        raise RuntimeError("cloudflared is required; run scripts/bootstrap-macos.sh first.")
    proc = subprocess.Popen([executable, "tunnel", "--url", PROXY_URL], cwd=ROOT)
    return ManagedProcess(pid=proc.pid, role="cloudflared", owned=True, popen=proc)


def stop_all(proxy: Optional[ManagedProcess], cloudflared: Optional[ManagedProcess], stop_adopted: bool):
    # Stop the public-facing tunnel first, then close its local target.
    for managed in (cloudflared, proxy):
        if managed is not None and (managed.owned or stop_adopted):
            try:
                managed.terminate()
            except (OSError, subprocess.SubprocessError) as exc:
                print(f"Could not stop {managed.role} PID {managed.pid}: {exc}", file=sys.stderr)


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--attach-cloudflared", type=int, metavar="PID", help="adopt an already-running cloudflared tunnel without changing its URL")
    parser.add_argument("--attach-proxy", type=int, metavar="PID", help="adopt the existing webhook proxy instead of starting another")
    return parser.parse_args()


def main() -> int:
    global _stopping
    args = parse_args()
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)

    proxy = None
    cloudflared = None
    pair_acquired = False
    try:
        if args.attach_cloudflared:
            cloudflared = validate_pid(args.attach_cloudflared, "cloudflared")
            print(f"Adopted cloudflared PID {cloudflared.pid}; existing public URL will remain unchanged.", flush=True)
        if args.attach_proxy:
            proxy = attach_proxy(args.attach_proxy)
            print(f"Adopted webhook proxy PID {proxy.pid} on {PROXY_URL}.", flush=True)
        else:
            proxy = make_proxy()
            print(f"Started webhook proxy PID {proxy.pid} on {PROXY_URL}.", flush=True)

        if cloudflared is None:
            cloudflared = make_cloudflared()
        pair_acquired = True
        print("Only POST /webhooks/github is forwarded. Setup and health endpoints remain local.", flush=True)

        while not _stopping:
            if not cloudflared.alive():
                raise RuntimeError("cloudflared exited; stopping the tunnel/proxy pair. The tunnel URL may need to be registered again.")
            if proxy is not None and (not proxy.alive() or not proxy_accepts_connections()):
                if proxy.alive():
                    print("Webhook proxy process is alive but its port is no longer accepting connections; replacing it.", file=sys.stderr, flush=True)
                    proxy.terminate()
                else:
                    print("Webhook proxy exited; restarting it while keeping cloudflared and its URL alive.", file=sys.stderr, flush=True)
                proxy = make_proxy()
                print(f"Restarted webhook proxy PID {proxy.pid} on {PROXY_URL}.", flush=True)
            time.sleep(POLL_SECONDS)
    except (RuntimeError, ValueError) as exc:
        print(f"tunnel supervisor: {exc}", file=sys.stderr, flush=True)
        return 1
    finally:
        stop_all(proxy, cloudflared, stop_adopted=pair_acquired)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
