"""Disposable Postgres for Docker smoke tests.

A pinned official image runs on a private Docker network; application
containers join it and read their connection URL from a mounted file. A restore
copies a database with pg_dump into a new one, as an operator's backup would.
"""
import os
import secrets
import shutil
import subprocess
import tempfile
import time
import uuid
from pathlib import Path

IMAGE = os.environ.get('SMOKE_POSTGRES_IMAGE', 'postgres:17.6-bookworm@sha256:f3bd19c606e442c3d7bdfa8002e03fe260a1023351e0ea4598032022b68dd6e3')


def _run(*args, input=None):
    return subprocess.run(args, input=input, text=True, capture_output=True, check=True).stdout.strip()


class Database:
    """One Postgres container; network names the existing network to join, or None.

    An internal network it creates has no route out, so its other containers
    reach only this database.
    """

    def __init__(self, network=None, internal=False):
        suffix = uuid.uuid4().hex[:10]
        self.name = 'forgetmenote-smoke-postgres-' + suffix
        self.owns_network = network is None
        self.network = network or 'forgetmenote-smoke-net-' + suffix
        self.urls = Path(tempfile.mkdtemp(prefix='fmn-smoke-db-'))
        # Containers run as another user; the password is synthetic and disposable.
        self.urls.chmod(0o755)
        self.password = secrets.token_hex(16)
        try:
            if self.owns_network:
                _run('docker', 'network', 'create', *(['--internal'] if internal else []), self.network)
            _run('docker', 'run', '-d', '--name', self.name, '--network', self.network,
                 '-e', 'POSTGRES_USER=forgetmenote', '-e', 'POSTGRES_PASSWORD=' + self.password,
                 '-e', 'POSTGRES_DB=forgetmenote', IMAGE)
            for _ in range(300):
                # Over the network: the image initializes through a temporary
                # server on its Unix socket alone, then restarts.
                if subprocess.run(['docker', 'exec', self.name, 'pg_isready', '-h', '127.0.0.1', '-U', 'forgetmenote', '-d', 'forgetmenote'],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError('smoke Postgres did not become ready')
            self._url('forgetmenote')
        except BaseException:
            self.close()
            raise

    def url(self, database='forgetmenote'):
        """The connection string a container on this network uses."""
        return f'postgres://forgetmenote:{self.password}@{self.name}:5432/{database}?sslmode=disable'

    def _url(self, database):
        path = self.urls / database
        path.write_text(self.url(database) + '\n')
        path.chmod(0o644)

    def args(self, database='forgetmenote'):
        """docker run arguments that give an application container this database."""
        return ['--network', self.network, '-e', 'DATABASE_URL_FILE=/run/database/' + database,
                '-v', str(self.urls) + ':/run/database:ro']

    def migrate(self, image, database='forgetmenote'):
        _run('docker', 'run', '--rm', *self.args(database), image, 'migrate')

    def restore_copy(self, database='restored', source='forgetmenote'):
        """An independent restore: a pg_dump of source loaded into a new database."""
        dump = _run('docker', 'exec', self.name, 'pg_dump', '-U', 'forgetmenote', '--no-owner', source)
        _run('docker', 'exec', self.name, 'createdb', '-U', 'forgetmenote', database)
        _run('docker', 'exec', '-i', self.name, 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-U', 'forgetmenote', '-d', database,
             input=dump + '\n')
        self._url(database)
        return database

    def close(self):
        subprocess.run(['docker', 'rm', '-f', self.name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if self.owns_network:
            subprocess.run(['docker', 'network', 'rm', self.network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        shutil.rmtree(self.urls, ignore_errors=True)
