"""Container smokes on a Linux host: private files, a booting Postgres, early refusals."""
import http.client
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


if __name__ == '__main__':
    unittest.main()
