"""The host port a smoke publishes its application container on, and proof
that what answers there is that container's application.

A smoke picks the port before its container starts and keeps it across
restarts and restores, so the port is unbound at those times. The kernel gives
an unbound port in its ephemeral range to any listen(0) or outgoing
connection, such as a concurrent Go test's httptest server in the release
gate, which would then answer the smoke in the application's place. So the
port comes from below the ephemeral range, and the application is ready only
once it accepts the recovery token in its own data directory.
"""
import http.client
import json
import random
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path


def free_port():
    """A port free on 127.0.0.1 below the kernel's ephemeral range, which
    listen(0) and outgoing connections never use."""
    if sys.platform == 'darwin':
        ephemeral = int(subprocess.check_output(['sysctl', '-n', 'net.inet.ip.portrange.first'], text=True))
    else:
        ephemeral = int(Path('/proc/sys/net/ipv4/ip_local_port_range').read_text().split()[0])
    if ephemeral <= 11000:
        raise RuntimeError(f'the ephemeral port range starts at {ephemeral}: no port below it is free')
    for _ in range(100):
        port = random.randrange(10000, ephemeral)
        with socket.socket() as sock:
            try:
                sock.bind(('127.0.0.1', port))
            except OSError:
                continue
        return port
    raise RuntimeError(f'no free port from 10000 to {ephemeral}')


def wait_ready(url, container, timeout, host=None):
    """Waits until the application in container answers at url: /api/me
    answers @local-admin for the recovery token in the container's /data.
    Another server on the port holds no such token, so it is never taken for
    the application. host is the Host header, for an instance whose public
    origin is not url."""
    deadline = time.monotonic() + timeout
    token, answer = '', 'nothing'
    while True:
        if not token:
            read = subprocess.run(['docker', 'exec', container, 'cat', '/data/admin.token'], text=True, capture_output=True)
            token, answer = read.stdout.strip(), read.stderr.strip() or 'no recovery token yet'
        if token:
            headers = {'Authorization': 'Bearer ' + token, **({'Host': host} if host else {})}
            try:
                with urllib.request.urlopen(urllib.request.Request(url + '/api/me', headers=headers), timeout=1) as response:
                    body = response.read()
                    me = json.loads(body)
                    if isinstance(me, dict) and me.get('id') == '@local-admin':
                        return
                    answer = f'{response.status} {body[:200]!r}'
            except urllib.error.HTTPError as error:
                answer = f'{error.code} {error.read()[:200]!r}'
                if error.code == 401:
                    token = ''  # Read before the container finished writing it.
            except (OSError, ValueError, http.client.HTTPException) as error:
                answer = repr(error)
        if time.monotonic() >= deadline:
            logs = subprocess.run(['docker', 'logs', '--tail', '40', container], text=True, capture_output=True)
            raise RuntimeError(f'{container} did not answer at {url} as its own application within {timeout} s; '
                               f'last answer: {answer}\n{logs.stdout}{logs.stderr}')
        time.sleep(0.1)
