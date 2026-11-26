"""Container smokes on a Linux host: private files, a booting Postgres, early refusals, an application port of their own."""
import http.client
import http.server
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
import threading
import time
import unittest
from unittest import mock

SCRIPTS = Path(__file__).resolve().parents[2] / 'scripts'
sys.path.insert(0, str(SCRIPTS))
import resource_smoke  # noqa: E402
import smoke_app  # noqa: E402
import smoke_database  # noqa: E402
import source_smoke  # noqa: E402
sys.path.remove(str(SCRIPTS))


def other_can(path, bits):
    return stat.S_IMODE(path.stat().st_mode) & bits == bits


class OfflineFixtureMounts(unittest.TestCase):
    """Docker Desktop hides file ownership from containers; a Linux host enforces it."""

    def setUp(self):
        previous = os.umask(0o077)
        self.addCleanup(os.umask, previous)
        self.mounted = {}

    def boundary(self, args, **options):
        if args[0] == 'go':
            output = Path(args[args.index('-o') + 1])
            output.write_bytes(b'test binary')
            output.chmod(0o700)  # What the Go tool leaves under the gate's umask.
        if '/checks/sources.test' in args and '--mount' in args:
            for index, value in enumerate(args):
                if value == '--mount':
                    fields = dict(field.split('=', 1) for field in args[index + 1].split(',') if '=' in field)
                    self.mounted[fields['target']] = self.modes(Path(fields['source']))
        return subprocess.CompletedProcess(args, 0, stdout='', stderr='')

    def modes(self, path):
        paths = [path, *sorted(path.rglob('*'))] if path.is_dir() else [path]
        return {str(item): (item.is_dir(), other_can(item, 0o005 if item.is_dir() or item.name.endswith('.test') else 0o004))
                for item in paths}

    def test_another_user_can_run_the_binary_and_read_the_fixtures(self):
        with mock.patch.object(source_smoke.subprocess, 'run', side_effect=self.boundary):
            source_smoke.offline_pdf_fixtures('sha256:image', full=False)
        self.assertEqual({'/checks/sources.test', '/fixtures'}, set(self.mounted))
        for target, modes in self.mounted.items():
            unreadable = [path for path, (_, readable) in modes.items() if not readable]
            self.assertEqual([], unreadable, target)
        self.assertTrue(any(path.endswith('testdata/iwork') for path in self.mounted['/fixtures']))

    def test_the_checkout_keeps_its_private_modes(self):
        fixtures = Path(source_smoke.__file__).resolve().parents[1] / 'internal/sources'
        before = stat.S_IMODE(fixtures.stat().st_mode)
        with mock.patch.object(source_smoke.subprocess, 'run', side_effect=self.boundary):
            source_smoke.offline_pdf_fixtures('sha256:image', full=False)
        self.assertEqual(before, stat.S_IMODE(fixtures.stat().st_mode))
        self.assertNotIn(str(fixtures), self.mounted['/fixtures'])


class DatabaseReadiness(unittest.TestCase):
    def test_a_database_is_ready_only_once_it_accepts_network_connections(self):
        # The image initializes through a temporary server on its Unix socket
        # alone, then restarts. A native application container connects first.
        checks = []

        def boundary(args, **options):
            code = 0
            if 'pg_isready' in args:
                checks.append(args)
                code = 0 if '-h' in args and len(checks) >= 3 else 2 if '-h' in args else 0
            return subprocess.CompletedProcess(args, code, stdout='', stderr='')

        with mock.patch.object(smoke_database.subprocess, 'run', side_effect=boundary), \
                mock.patch.object(smoke_database.time, 'sleep'):
            smoke_database.Database(network='existing').close()
        self.assertGreaterEqual(len(checks), 3)
        self.assertIn('127.0.0.1', checks[-1])


class EarlyRefusal(unittest.TestCase):
    """A server at capacity answers before reading a large upload, then closes."""

    def serve(self, reply):
        listener = socket.create_server(('127.0.0.1', 0))
        self.addCleanup(listener.close)

        def respond():
            connection, _ = listener.accept()
            with connection:
                connection.recv(1024)
                connection.sendall(reply)
                # Go's server half-closes, waits briefly, then closes with the
                # upload unread, which resets the client still writing it.
                connection.shutdown(socket.SHUT_WR)
                time.sleep(.2)

        thread = threading.Thread(target=respond, daemon=True)
        thread.start()
        self.addCleanup(thread.join, 5)
        return listener.getsockname()[1]

    def test_a_refusal_sent_during_the_upload_is_read(self):
        reply = b'HTTP/1.1 429 Too Many Requests\r\nContent-Length: 8\r\nConnection: close\r\n\r\ncapacity'
        status, body = resource_smoke.send(self.serve(reply), 'token', '/upload', {'data': 'x' * (32 << 20)})
        self.assertEqual((429, b'capacity'), (status, body))

    def test_a_connection_lost_without_a_reply_still_fails(self):
        with self.assertRaises((ConnectionError, http.client.HTTPException)):
            resource_smoke.send(self.serve(b''), 'token', '/upload', {'data': 'x' * (32 << 20)})


class ApplicationPort(unittest.TestCase):
    """A smoke's application port is unbound until its container starts, and
    again while it restarts or is restored. A Go test's httptest server in the
    same gate once took the browser harness's port in such a gap and answered
    its readiness probe and account setup with 200."""

    def serve(self, answer):
        """A server on a port of its own; answer(path, headers) gives each reply."""
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                status, body = answer(self.path, self.headers)
                self.send_response(status)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join, 5)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f'http://127.0.0.1:{server.server_port}'

    def container(self, *tokens):
        """docker: each read of the container's admin.token gives the next of tokens."""
        reads = iter(tokens)

        def boundary(args, **options):
            stdout = next(reads, tokens[-1]) if args[-1] == '/data/admin.token' else ''
            return subprocess.CompletedProcess(args, 0, stdout=stdout, stderr='')
        return mock.patch.object(smoke_app.subprocess, 'run', side_effect=boundary)

    def test_the_port_is_one_the_kernel_never_hands_to_another_process(self):
        if sys.platform == 'darwin':
            ephemeral = int(subprocess.check_output(['sysctl', '-n', 'net.inet.ip.portrange.first'], text=True))
        else:
            ephemeral = int(Path('/proc/sys/net/ipv4/ip_local_port_range').read_text().split()[0])
        self.assertLess(smoke_app.free_port(), ephemeral)

    def test_another_server_on_the_port_is_never_taken_for_the_application(self):
        url = self.serve(lambda path, headers: (200, b'{"id":"someone-else"}'))
        with self.container('synthetic-recovery-token'), self.assertRaisesRegex(RuntimeError, 'someone-else'):
            smoke_app.wait_ready(url, 'synthetic-container', 1)

    def test_the_application_is_ready_once_it_accepts_its_own_recovery_token(self):
        def answer(path, headers):
            if path == '/healthz':
                return 200, b'ok'
            if path == '/api/me' and headers['Authorization'] == 'Bearer synthetic-recovery-token':
                return 200, b'{"id":"@local-admin"}'
            return 401, b'unauthorized'

        # The container has not written its token on the first read.
        with self.container('', 'synthetic-recovery-token'):
            smoke_app.wait_ready(self.serve(answer), 'synthetic-container', 5)


if __name__ == '__main__':
    unittest.main()
