#!/usr/bin/env python3
"""Run a command inside a Plasma VM over its serial console.

A stopgap until `ne-agent` (M2) lands and gives us `plasma vm exec`. It drives
the same console socket `plasma vm shell` attaches to: log in, run a command,
capture its output and exit status, then log out.

Usage:
    vm-exec.py <vm-id> "<command>" [--timeout SECS]
"""

import argparse
import json
import os
import re
import socket
import subprocess
import sys
import time

SOCKET = "/var/run/plasma/vms/{vm}/console.sock"
STATE = "/var/lib/plasma/state.json"
USER = "root"
PASSWORD = "plasma"

LOGIN_RE = re.compile(r"login:\s*$")
PASSWORD_RE = re.compile(r"Password:\s*$")
# Match the real prompt, not a stray '#' in the MOTD.
PROMPT_RE = re.compile(r"root@[\w.-]+:[^\r\n]*#\s*$")
ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")

BEGIN = "___NEBEGIN___"
END = "___NEEND___"


def expect(sock, pattern, timeout, label):
    """Read until `pattern` matches the accumulated text."""
    buf = ""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        sock.settimeout(max(0.1, deadline - time.monotonic()))
        try:
            chunk = sock.recv(4096)
        except socket.timeout:
            continue
        except OSError:
            break
        if not chunk:
            break
        # Strip escapes from the accumulated text, not per chunk: a sequence
        # split across two reads would otherwise slip through half-erased.
        buf += chunk.decode("utf-8", "replace")
        clean = ANSI_RE.sub("", buf)
        if pattern.search(clean):
            return clean
    raise TimeoutError(f"timed out waiting for {label}; last saw:\n{buf[-500:]}")


class PtyConn:
    """Give a pty the same recv/sendall/close surface as a socket."""

    def __init__(self, path):
        self.fd = os.open(path, os.O_RDWR | os.O_NONBLOCK)
        # Stop the pty's line discipline echoing what we type back at us, on
        # top of the guest's own echo.
        subprocess.run(["stty", "-F", path, "raw", "-echo"], check=False)
        self._timeout = 1.0

    def settimeout(self, t):
        self._timeout = t

    def recv(self, n):
        deadline = time.monotonic() + self._timeout
        while time.monotonic() < deadline:
            try:
                return os.read(self.fd, n)
            except BlockingIOError:
                time.sleep(0.05)
        raise socket.timeout()

    def sendall(self, data):
        os.write(self.fd, data)

    def close(self):
        try:
            os.close(self.fd)
        except OSError:
            pass


def connect_console(vm):
    """Attach to a VM's console: pty for new VMs, unix socket for older ones."""
    pty = None
    try:
        with open(STATE) as f:
            for rec in json.load(f):
                if rec.get("id") == vm:
                    pty = rec.get("console_pty") or None
                    break
    except (OSError, ValueError):
        pass  # fall back to the socket path

    if pty and os.path.exists(pty):
        try:
            return PtyConn(pty)
        except OSError as e:
            sys.exit(f"open {pty}: {e} (are you in the plasma group?)")

    path = SOCKET.format(vm=vm)
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(path)
    except OSError as e:
        sys.exit(f"connect {path}: {e} (VM running? are you in the plasma group?)")
    return s


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("vm")
    ap.add_argument("command")
    ap.add_argument("--timeout", type=float, default=60.0)
    args = ap.parse_args()

    s = connect_console(args.vm)

    try:
        # getty printed its prompt to nobody before we attached; a newline
        # makes it reprint.
        s.sendall(b"\n")
        seen = expect(s, re.compile(r"(login:\s*$)|(root@[\w.-]+:[^\r\n]*#\s*$)"),
                      25, "login prompt")

        if LOGIN_RE.search(seen):
            s.sendall((USER + "\n").encode())
            expect(s, PASSWORD_RE, 20, "password prompt")
            s.sendall((PASSWORD + "\n").encode())
            expect(s, PROMPT_RE, 30, "shell prompt")

        # Keep kernel messages out of the captured output.
        s.sendall(b"dmesg -n 1 2>/dev/null\n")
        expect(s, PROMPT_RE, 15, "prompt after dmesg -n 1")

        s.sendall(f"echo {BEGIN}; {args.command}\necho {END}$?\n".encode())
        out = expect(s, re.compile(re.escape(END) + r"\d+"), args.timeout, "command output")
    except TimeoutError as e:
        s.close()
        sys.exit(str(e))

    # Deliberately NOT logging out. Sending `exit` makes agetty respawn and
    # write to a serial socket whose client has gone; cloud-hypervisor keeps
    # the stale connection, the guest blocks, and the console is unusable for
    # every later client. Leaving the session logged in keeps it reattachable.
    s.close()

    # Take the LAST occurrence: the command line itself is echoed back first.
    starts = [m.end() for m in re.finditer(re.escape(BEGIN) + r"\r?\n", out)]
    m_end = list(re.finditer(re.escape(END) + r"(\d+)", out))
    if not starts or not m_end:
        sys.exit("could not parse console output:\n" + out[-1200:])

    body = out[starts[-1]:m_end[-1].start()]
    # The tail carries the shell echoing back our own 'echo ___NEEND___$?'
    # line, prompt and all. Cut from the last prompt onward.
    cut = list(re.finditer(r"root@[\w.-]+:[^\r\n]*#", body))
    if cut:
        body = body[:cut[-1].start()]
    print(body.replace("\r\n", "\n").rstrip())
    sys.exit(int(m_end[-1].group(1)))


if __name__ == "__main__":
    main()
