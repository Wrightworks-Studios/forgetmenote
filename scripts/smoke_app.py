"""The application container a smoke scenario starts: its host port and readiness.

A scenario picks the port before the container binds it, and every restart
frees it again. The gate runs scenarios beside its host tests, and the kernel
can give a port from its ephemeral range to one of them in between, as it gave
the browser harness's port to a Go test's httptest server, which then answered
in the app's place. The kernel hands out ports to listen(0) and outgoing
connections only from that range, so the app takes a free port below it, and
ready means the app's own answer, not just something answering on the port.
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

LOWEST = 10000


def ephemeral_start():
    """The first port of the kernel's ephemeral range."""
    if sys.platform == 'darwin':
        return int(subprocess.check_output(['sysctl', '-n', 'net.inet.ip.portrange.first'], text=True))
    return int(Path('/proc/sys/net/ipv4/ip_local_port_range').read_text().split()[0])


def app_port():
    """A free loopback port below the ephemeral range, for the app's container to publish."""
    ephemeral = ephemeral_start()
    if ephemeral <= LOWEST + 1000:
        raise RuntimeError(f'The ephemeral port range starts at {ephemeral}: no port below it is free')
    for _ in range(100):
        port = random.randrange(LOWEST, ephemeral)
        with socket.socket() as sock:
            try:
                sock.bind(('127.0.0.1', port))
            except OSError:
                continue
        return port
    raise RuntimeError(f'No free port from {LOWEST} to {ephemeral}')


def container_token(container):
    """Reads the recovery token the app in container writes as it starts, or ''
    before then. Never forgetmenote token, which writes one if there is none."""
    def read():
        result = subprocess.run(['docker', 'exec', container, 'cat', '/data/admin.token'], text=True, capture_output=True)
        return result.stdout.strip() if result.returncode == 0 else ''
    return read


def wait_app(origin, token, host=None, timeout=60):
    """Waits until the app answers at origin as itself, and returns its recovery token.

    Only the app accepts its own recovery token, so ready is /api/me answering
    as the recovery administrator. token is that token, or a function reading
    it, such as container_token. host is the Host header the app expects when
    origin is not its public URL.
    """
    # Every answer comes straight from origin, never through a configured proxy.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + timeout
    while True:
        try:
            current = token() if callable(token) else token
            if not current:
                last = 'the app has not written its recovery token'
            else:
                token = current  # Read it once the app has written it.
                headers = {'Authorization': 'Bearer ' + current, **({'Host': host} if host else {})}
                with opener.open(urllib.request.Request(origin + '/api/me', headers=headers), timeout=5) as response:
                    identity = json.load(response).get('id')
                if identity == '@local-admin':
                    return current
                last = f'/api/me answered as {identity!r}'
        except urllib.error.HTTPError as error:
            error.close()
            last = f'/api/me answered HTTP {error.code}'
        except (OSError, http.client.HTTPException, ValueError, AttributeError) as error:
            last = f'{type(error).__name__}: {error}'
        if time.monotonic() >= deadline:
            raise RuntimeError(f'{origin} did not answer as the app within {timeout} s: {last}')
        time.sleep(0.1)
