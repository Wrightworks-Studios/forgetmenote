#!/usr/bin/env python3
"""Offline operator for the optional Authelia bundle; never imported by ForgetMeNote."""
import argparse
import base64
import fcntl
import getpass
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile

import yaml
from argon2 import PasswordHasher

AUTHELIA = 'authelia/authelia:4.39.22'
CADDY = 'caddy:2.10.2-alpine'
# The instance database; upgrade its major version deliberately, after a backup.
# Pinned to the multi-platform index digest, so every host pulls the same build.
POSTGRES = 'postgres:17.6-bookworm@sha256:f3bd19c606e442c3d7bdfa8002e03fe260a1023351e0ea4598032022b68dd6e3'
REPO = Path(__file__).resolve().parents[2]


def atomic(path, value, replace=False):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if path.exists() and not replace:
        return
    fd, name = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as f:
            f.write(value)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def secret(root, name):
    path = root / 'secrets' / name
    atomic(path, secrets.token_urlsafe(48))
    return path.read_text().strip()


def client_hash(password):
    salt = secrets.token_bytes(16)
    digest = hashlib.pbkdf2_hmac('sha512', password.encode(), salt, 310000)
    b64 = lambda b: base64.b64encode(b).decode().rstrip('=').replace('+', '.')
    return '$pbkdf2-sha512$310000$' + b64(salt) + '$' + b64(digest)


def notification_mode(settings):
    # Existing bundles without this field used SMTP; never silently change them.
    return settings.get('notification_mode', 'smtp')


def configure_notifications(root, config, settings):
    mode = notification_mode(settings)
    if mode not in ['operator', 'smtp']:
        raise ValueError('Notification mode must be operator or smtp')
    config['authentication_backend'].setdefault('password_reset', {})['disable'] = mode == 'operator'
    assets = '/config/forgetmenote-assets'
    current = config['server'].get('asset_path', '')
    if current not in ['', assets, '/config/gitnotes-assets']:
        raise ValueError('Custom Authelia assets are configured; merge the email notices manually before changing mode')
    if mode == 'smtp':
        config['notifier'] = {'smtp': {'address': settings['smtp_address'], 'username': settings['smtp_username'], 'password': (root / 'secrets/smtp-password').read_text().strip(), 'sender': settings['smtp_sender']}}
        config['server'].pop('asset_path', None)
    else:
        config['notifier'] = {'filesystem': {'filename': '/config/notification.txt'}}
        config['server']['asset_path'] = assets
        overrides = json.loads((REPO / 'deploy/standalone/no-email.json').read_text())
        for locale in overrides['locales']:
            for namespace, messages in overrides['messages'].items():
                atomic(root / 'authelia/forgetmenote-assets/locales' / locale / (namespace + '.json'), json.dumps(messages, indent=2), replace=True)
        if os.geteuid() == 0:
            folder = root / 'authelia/forgetmenote-assets'
            for path in [folder, *folder.rglob('*')]:
                os.chown(path, settings['uid'], settings['gid'])


def smtp_settings():
    settings = {key: input(prompt).strip() for key, prompt in [
        ('smtp_address', 'SMTP address (submission://host:587 or submissions://host:465): '),
        ('smtp_username', 'SMTP username: '), ('smtp_sender', 'From address: ')]}
    if not settings['smtp_address'].startswith(('submission://', 'submissions://')) or not settings['smtp_username'] or '@' not in settings['smtp_sender']:
        raise ValueError('Authenticated TLS SMTP and a valid sender are required')
    return settings


def smtp_password(root):
    value = getpass.getpass('SMTP password: ')
    if not value:
        raise ValueError('SMTP password is required')
    atomic(root / 'secrets/smtp-password', value, replace=True)


def set_notifications(root, mode):
    require_stopped(root)
    settings = json.loads((root / 'settings.json').read_text())
    config = yaml.safe_load((root / 'authelia/configuration.yml').read_text())
    if mode not in ['operator', 'smtp']:
        raise ValueError('Use notifications operator or notifications smtp')
    settings['notification_mode'] = mode
    if mode == 'smtp':
        settings.update(smtp_settings())
        smtp_password(root)
    configure_notifications(root, config, settings)
    atomic(root / 'authelia/configuration.yml', yaml.safe_dump(config, sort_keys=False), replace=True)
    atomic(root / 'settings.json', json.dumps(settings, indent=2), replace=True)
    if os.geteuid() == 0:
        os.chown(root / 'authelia/configuration.yml', settings['uid'], settings['gid'])
    print('Email disabled. Enrollment and recovery require the server operator; configure SMTP to enable email.' if mode == 'operator' else 'SMTP configured. Validate and restart Authelia, then verify delivery before relying on email recovery.')


def enroll_totp(root, name):
    users = yaml.safe_load((root / 'authelia/users.yml').read_text())['users']
    if name not in users or users[name].get('disabled', False):
        raise ValueError('An existing enabled Authelia account is required')
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}', name):
        raise ValueError('Invalid account name')
    target = root / 'authelia/enrollment' / (name + '.png')
    if target.exists():
        raise ValueError('Private enrollment QR already exists: ' + str(target) + '. It has not been replaced.')
    target.parent.mkdir(exist_ok=True, mode=0o700)
    settings = json.loads((root / 'settings.json').read_text())
    if os.geteuid() == 0:
        os.chown(target.parent, settings['uid'], settings['gid'])
    # No --force: the provider refuses to overwrite an existing TOTP registration.
    # Capture output because the provider prints the shared secret even with --path.
    try:
        compose(root, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'storage', 'user', 'totp', 'generate', name, '--config', '/config/configuration.yml', '--path', '/config/enrollment/' + name + '.png', capture=True)
    finally:
        if target.exists():
            target.chmod(0o600)
    print('Private authenticator QR: ' + str(target))
    print('Scan locally, verify a password + authenticator login, then delete this QR file. Never commit or share it publicly. No email was sent.')


def render(root, s):
    """Create missing files only. Existing configuration and secrets always win."""
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    for name in ['secrets', 'authelia', 'data', 'postgres', 'caddy-data', 'caddy-config']:
        (root / name).mkdir(exist_ok=True, mode=0o700)
    atomic(root / 'settings.json', json.dumps(s, indent=2))
    atomic(root / '.gitignore', '*\n')
    client = secret(root, 'oidc-client-secret')
    # URL-safe, so the password needs no escaping inside the URL.
    atomic(root / 'secrets' / 'database-url', 'postgres://forgetmenote:' + secret(root, 'database-password') + '@postgres:5432/forgetmenote?sslmode=disable\n')
    key = root / 'secrets' / 'oidc.pem'
    if not key.exists():
        result = subprocess.run(['openssl', 'genpkey', '-algorithm', 'RSA', '-pkeyopt', 'rsa_keygen_bits:3072'], capture_output=True, text=True, check=True)
        atomic(key, result.stdout)
    if not (root / 'authelia/configuration.yml').exists():
        config = {
            'server': {'address': 'tcp://:9091/', 'disable_healthcheck': True},
            'log': {'level': 'info'},
            'totp': {'disable': False, 'issuer': s['domain']},
            'authentication_backend': {'file': {'path': '/config/users.yml', 'watch': False}},
            'access_control': {'default_policy': 'one_factor'},
            'session': {'secret': secret(root, 'session'), 'expiration': '1h', 'inactivity': '15m', 'remember_me': '0', 'cookies': [{'domain': s['auth_domain'], 'authelia_url': 'https://' + s['auth_domain']}]},
            'storage': {'encryption_key': secret(root, 'storage'), 'local': {'path': '/config/db.sqlite3'}},
            'identity_validation': {'reset_password': {'jwt_secret': secret(root, 'reset-password')}},
            'notifier': {},
            'identity_providers': {'oidc': {
                'hmac_secret': secret(root, 'oidc-hmac'),
                'jwks': [{'key_id': 'forgetmenote', 'algorithm': 'RS256', 'use': 'sig', 'key': key.read_text()}],
                'clients': [{'client_id': 'forgetmenote', 'client_name': 'ForgetMeNote', 'client_secret': client_hash(client), 'public': False, 'authorization_policy': 'one_factor', 'require_pkce': True, 'pkce_challenge_method': 'S256', 'redirect_uris': ['https://' + s['domain'] + '/auth/oidc/callback'], 'scopes': ['openid'], 'grant_types': ['authorization_code'], 'response_types': ['code'], 'response_modes': ['query'], 'token_endpoint_auth_method': 'client_secret_basic'}]
            }}
        }
        configure_notifications(root, config, s)
        atomic(root / 'authelia/configuration.yml', yaml.safe_dump(config, sort_keys=False))
    uid = f"{s['uid']}:{s['gid']}"
    compose = {'services': {
        'postgres': {'image': POSTGRES, 'user': uid, 'restart': 'unless-stopped', 'stop_grace_period': '60s', 'environment': {'POSTGRES_USER': 'forgetmenote', 'POSTGRES_DB': 'forgetmenote', 'POSTGRES_PASSWORD_FILE': '/run/secrets/database-password'}, 'volumes': ['./postgres:/var/lib/postgresql/data', './secrets/database-password:/run/secrets/database-password:ro'], 'healthcheck': {'test': ['CMD', 'pg_isready', '-U', 'forgetmenote', '-d', 'forgetmenote'], 'interval': '5s', 'timeout': '5s', 'retries': 30}, 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true']},
        # Serving never migrates: apply this binary's schema, then serve.
        'forgetmenote': {'image': 'forgetmenote:standalone', 'build': {'context': str(REPO)}, 'user': uid, 'restart': 'unless-stopped', 'init': True, 'stop_grace_period':'30s', 'entrypoint': ['/bin/sh', '-c', 'forgetmenote migrate && exec forgetmenote "$$@"', 'forgetmenote'], 'depends_on': {'postgres': {'condition': 'service_healthy'}}, 'environment': {'PUBLIC_URL': 'https://' + s['domain'], 'OIDC_ISSUER_URL': 'https://' + s['auth_domain'], 'OIDC_CLIENT_ID': 'forgetmenote', 'OIDC_CLIENT_SECRET_FILE': '/run/secrets/oidc-client-secret', 'DATABASE_URL_FILE': '/run/secrets/database-url'}, 'volumes': ['./data:/data', './secrets/oidc-client-secret:/run/secrets/oidc-client-secret:ro', './secrets/database-url:/run/secrets/database-url:ro'], 'read_only': True, 'tmpfs': ['/tmp:size=64m,mode=1777'], 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true']},
        'authelia': {'image': AUTHELIA, 'healthcheck': {'test': ['CMD', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:9091/api/health'], 'interval': '30s', 'timeout': '5s', 'start_period': '15s'}, 'user': uid, 'restart': 'unless-stopped', 'volumes': ['./authelia:/config'], 'read_only': True, 'tmpfs': ['/tmp:size=64m,mode=1777'], 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true']},
        'caddy': {'image': CADDY, 'user': uid, 'sysctls': {'net.ipv4.ip_unprivileged_port_start': '0'}, 'restart': 'unless-stopped', 'ports': ['80:80', '443:443'], 'volumes': ['./Caddyfile:/etc/caddy/Caddyfile:ro', './caddy-data:/data', './caddy-config:/config'], 'depends_on': ['forgetmenote', 'authelia'], 'security_opt': ['no-new-privileges:true']}
    }}
    atomic(root / 'compose.yaml', yaml.safe_dump(compose, sort_keys=False))
    atomic(root / 'Caddyfile', s['domain'] + ' {\n reverse_proxy forgetmenote:8080\n}\n' + s['auth_domain'] + ' {\n reverse_proxy authelia:9091\n}\n')
    if os.geteuid() == 0:
        for folder in ['data', 'authelia', 'secrets', 'postgres', 'caddy-data', 'caddy-config']:
            for path in [root / folder, *(root / folder).rglob('*')]:
                os.chown(path, s['uid'], s['gid'])
        os.chown(root / 'Caddyfile', s['uid'], s['gid'])


def compose(root, *args, capture=False):
    return subprocess.run(['docker', 'compose', '-f', str(root / 'compose.yaml'), *args], check=True, capture_output=capture, text=True)


def app_service(root):
    services = yaml.safe_load((root / 'compose.yaml').read_text())['services']
    return 'forgetmenote' if 'forgetmenote' in services else 'gitnotes'


def require_stopped(root):
    if compose(root, 'ps', '-q', 'authelia', capture=True).stdout.strip():
        raise ValueError('Stop Authelia before account maintenance: docker compose -f ' + str(root / 'compose.yaml') + ' stop authelia')


def password():
    a = getpass.getpass('New Authelia password: ')
    b = getpass.getpass('Confirm password: ')
    if a != b or len(a) < 12:
        raise ValueError('Passwords must match and contain at least 12 characters')
    return PasswordHasher(time_cost=3, memory_cost=65536, parallelism=4).hash(a)


def user_change(root, action, name, email=None, hashed=None):
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}', name):
        raise ValueError('Use a simple stable Authelia username; usernames cannot be renamed')
    path = root / 'authelia/users.yml'
    if not path.exists() and (root / 'initial-account.created').exists():
        raise ValueError('Authelia user database is missing; restore it from backup instead of recreating identities')
    data = yaml.safe_load(path.read_text()) if path.exists() else {'users': {}}
    users = data['users']
    if action == 'add':
        if name in users:
            raise ValueError('Account already exists; use password, email, disable or enable')
        users[name] = {'displayname': name, 'password': hashed, 'email': email, 'disabled': False, 'groups': []}
    else:
        if name not in users:
            raise ValueError('Unknown account')
        if action in ['disable', 'enable']:
            users[name]['disabled'] = action == 'disable'
        elif action == 'password':
            users[name]['password'] = hashed
        elif action == 'email':
            users[name]['email'] = email
    atomic(path, yaml.safe_dump(data, sort_keys=False), replace=True)
    s = json.loads((root / 'settings.json').read_text())
    if os.geteuid() == 0:
        os.chown(path, s['uid'], s['gid'])


def setup(root):
    if (root / 'settings.json').exists():
        s = json.loads((root / 'settings.json').read_text())
    else:
        domain = input('ForgetMeNote domain (for example notes.example.com): ').strip().lower()
        auth = input('Authelia domain [auth.' + domain + ']: ').strip().lower() or 'auth.' + domain
        for host in [domain, auth]:
            if not re.fullmatch(r'[a-z0-9]+(?:[.-][a-z0-9]+)+', host):
                raise ValueError('Enter DNS hostnames only')
        if domain == auth:
            raise ValueError('ForgetMeNote and Authelia require different hostnames')
        mode = input('Notifications: operator (no email) or smtp [operator]: ').strip().lower() or 'operator'
        if mode not in ['operator', 'smtp']:
            raise ValueError('Choose operator or smtp')
        s = {'domain': domain, 'auth_domain': auth, 'notification_mode': mode, 'admin': input('Initial administrator Authelia username: ').strip(), 'admin_email': input('Account email (optional without SMTP): ').strip(), 'uid': os.getuid() or 1000, 'gid': os.getgid() or 1000}
        if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}', s['admin']):
            raise ValueError('Use a simple stable Authelia username')
        if (s['admin_email'] or mode == 'smtp') and '@' not in s['admin_email']:
            raise ValueError('A valid email is required for SMTP enrollment and recovery')
        if mode == 'smtp':
            s.update(smtp_settings())
        atomic(root / 'settings.json', json.dumps(s, indent=2))
    if notification_mode(s) == 'smtp' and not (root / 'secrets/smtp-password').exists():
        smtp_password(root)
    render(root, s)
    if not (root / 'authelia/users.yml').exists() and (root / 'initial-account.created').exists():
        raise ValueError('Authelia user database is missing; restore it from backup')
    if not (root / 'authelia/users.yml').exists():
        user_change(root, 'add', s['admin'], s['admin_email'], password())
    atomic(root / 'initial-account.created', s['admin'] + '\n')
    compose(root, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'config', 'validate', '--config', '/config/configuration.yml', capture=True)
    print('Configuration ready. No ForgetMeNote Admin has been granted automatically.')
    if notification_mode(s) == 'operator':
        print('Email is DISABLED. Sign in with your Authelia username and password. TOTP enrollment is optional (enroll-totp USER); the OIDC client does not require it. Configure notifications smtp for email functionality.')
    else:
        print('Start Compose, verify SMTP delivery, then sign in through OIDC. Two-factor authentication is optional unless the provider policy explicitly requires it.')
    print('Copy your User ID from User settings, stop ForgetMeNote, then run this command with grant-admin USER_ID. Verify that it is your own signed-in account.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, required=True, help='Private deployment directory outside the source repository')
    parser.add_argument('action', choices=['setup', 'add', 'password', 'email', 'disable', 'enable', 'reset-totp', 'enroll-totp', 'notifications', 'grant-admin'])
    parser.add_argument('user', nargs='?')
    a = parser.parse_args()
    root = a.directory.expanduser().resolve()
    if root == REPO or REPO in root.parents:
        raise ValueError('Keep deployment data and secrets outside the source repository')
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root / '.operator.lock').open('a') as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if a.action == 'setup':
            setup(root)
            return
        if not a.user:
            raise ValueError('Specify a user ID')
        if a.action == 'notifications':
            set_notifications(root, a.user)
            return
        if a.action == 'grant-admin':
            # The application takes the data-directory lock and rejects an active writer.
            compose(root, 'run', '--rm', '--no-deps', app_service(root), 'grant-admin', a.user)
        else:
            require_stopped(root)
            if a.action == 'enroll-totp':
                enroll_totp(root, a.user)
            elif a.action == 'reset-totp':
                compose(root, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'storage', 'user', 'totp', 'delete', a.user, '--config', '/config/configuration.yml', capture=True)
                if re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}', a.user):
                    (root / 'authelia/enrollment' / (a.user + '.png')).unlink(missing_ok=True)
            else:
                email = input('Account email: ').strip() if a.action in ['add', 'email'] else None
                mode = notification_mode(json.loads((root / 'settings.json').read_text()))
                if email is not None and (email or mode == 'smtp') and '@' not in email:
                    raise ValueError('A valid email is required when SMTP is enabled')
                hashed = password() if a.action in ['add', 'password'] else None
                user_change(root, a.action, a.user, email, hashed)
            print('Authelia account updated. Start Authelia again. Disable/revoke the ForgetMeNote user separately to end existing browser and MCP access.')

if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as e:
        # Never print captured provider output: it may include configuration secrets.
        raise SystemExit(str(e) if not isinstance(e, subprocess.CalledProcessError) else 'Operator command failed; check local configuration and service status (secrets were not printed).')
