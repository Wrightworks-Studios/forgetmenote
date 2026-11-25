#!/usr/bin/env python3
"""Print a GitHub App manifest, or register it through an operator-only local callback."""
import argparse
import hmac
from http.server import BaseHTTPRequestHandler, HTTPServer
import html
import json
import os
from pathlib import Path
import re
import secrets
import time
import urllib.parse
import urllib.request


def manifest(kind, origin, name, allow_create=False, public=False):
    url = urllib.parse.urlsplit(origin)
    if not url.hostname or url.path or url.query or url.fragment or url.username or (url.scheme != 'https' and not (url.scheme == 'http' and url.hostname in ['localhost', '127.0.0.1'])):
        raise ValueError('Use an HTTPS origin, or HTTP localhost for development')
    if kind not in ['sources', 'backups'] or allow_create and kind != 'backups':
        raise ValueError('Only the Backups App can request repository creation')
    prefix = 'repository' if kind == 'sources' else 'backup'
    permissions = {'metadata':'read', 'contents':'read' if kind == 'sources' else 'write'}
    if allow_create:
        permissions['administration'] = 'write'
    value = dict(name=name, url=origin, callback_urls=[origin+'/'+prefix+'/github/callback'],
                setup_url=origin, request_oauth_on_install=False, setup_on_update=True,
                public=public, default_permissions=permissions,
                default_events=[])
    # GitHub validates even inactive hook URLs. Local Sources uses polling;
    # Backups checks authority at each lease and never needs a webhook.
    if kind == 'sources' and url.hostname not in ['localhost', '127.0.0.1']:
        value['default_events'] = ['push','repository']
        value['hook_attributes'] = {'url':origin+'/repository/github/webhook', 'active':True}
    return value


def save_credentials(directory, response, kind, allow_create):
    # An interrupted registration leaves its private files for the operator to
    # recover; rerunning must never silently replace another App's credentials.
    values = {'private-key.pem':response['pem'], 'client-secret':response['client_secret'],
              'webhook-secret':response.get('webhook_secret') or secrets.token_urlsafe(32)}
    config = dict(appId=str(response['id']), slug=response['slug'], clientId=response['client_id'],
                  privateKeyFile=str(directory/'private-key.pem'), clientSecretFile=str(directory/'client-secret'),
                  webhookSecretFile=str(directory/'webhook-secret'))
    if kind == 'backups':
        values['worker-token'] = secrets.token_urlsafe(48)
        config = dict(app=config, allowCreate=allow_create, workers={'local':{'owner':'local','tokenFile':str(directory/'worker-token')}})
    values['app.json'] = json.dumps(config, indent=2)
    for name, value in values.items():
        with open(directory/name, 'x', opener=lambda p, f: os.open(p, f, 0o600)) as out:
            out.write(value+'\n'); out.flush(); os.fsync(out.fileno())


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def register(args, value):
    directory = Path(args.secrets_dir).resolve()
    if any((p/'.git').exists() for p in [directory, *directory.parents]):
        raise ValueError('The secret directory must be outside every Git repository')
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    nonce, state = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    complete, failure = [], []
    registration = 'https://github.com/settings/apps/new'
    if args.org:
        if not re.fullmatch('[A-Za-z0-9-]+', args.org):
            raise ValueError('Invalid organization name')
        registration = 'https://github.com/organizations/'+args.org+'/settings/apps/new'
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass  # Callback codes and state never enter logs.
        def do_GET(self):
            url = urllib.parse.urlsplit(self.path)
            self.send_response(200)
            self.send_header('Content-Type','text/html; charset=utf-8')
            self.send_header('Cache-Control','no-store')
            self.send_header('Referrer-Policy','no-referrer')
            self.send_header('X-Content-Type-Options','nosniff')
            self.send_header('Content-Security-Policy',"default-src 'none'; form-action https://github.com; frame-ancestors 'none'")
            self.end_headers()
            if self.headers.get('Host') != '127.0.0.1:'+str(server.server_port):
                self.wfile.write(b'Invalid host'); return
            if url.path == '/'+nonce:
                form='<h1>Register '+html.escape(args.name)+'</h1><pre>'+html.escape(json.dumps(value,indent=2))+'</pre><form method="post" action="'+registration+'?state='+state+'"><input type="hidden" name="manifest" value="'+html.escape(json.dumps(value),quote=True)+'"><button>Create this GitHub App</button></form>'
                self.wfile.write(form.encode()); return
            query = urllib.parse.parse_qs(url.query)
            supplied = query.get('state',[''])[0]; code = query.get('code',[''])[0]
            if url.path != '/callback' or not hmac.compare_digest(supplied,state) or not re.fullmatch('[A-Za-z0-9_-]{10,200}',code) or complete or failure:
                self.wfile.write(b'Invalid or expired registration'); return
            try:
                req=urllib.request.Request('https://api.github.com/app-manifests/'+code+'/conversions',data=b'',headers={'Accept':'application/vnd.github+json','X-GitHub-Api-Version':'2022-11-28'})
                http=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
                with http.open(req,timeout=30) as response:
                    raw=response.read(131073)
                    if len(raw)>131072:raise ValueError('Invalid GitHub response')
                    result=json.loads(raw)
                save_credentials(directory,result,args.kind,args.allow_create)
                complete.append(True);self.wfile.write(b'App credentials saved privately. Return to the terminal for the configuration path.')
            except Exception:
                failure.append(True);self.wfile.write(b'Registration could not be completed. Inspect the private output directory and GitHub App settings; no credentials are displayed here.')
    with HTTPServer(('127.0.0.1',0),Handler) as server:
        server.timeout=1
        value['redirect_url']='http://127.0.0.1:'+str(server.server_port)+'/callback'
        print('Review and register: http://127.0.0.1:'+str(server.server_port)+'/'+nonce,flush=True)
        deadline=time.monotonic()+3600
        while not complete and not failure and time.monotonic()<deadline:
            server.handle_request()
    if not complete:
        raise RuntimeError('Registration incomplete; check GitHub App settings and the private secret directory')
    print('Configuration: '+str(directory/'app.json'))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('kind',choices=['sources','backups'])
    parser.add_argument('--origin',required=True)
    parser.add_argument('--name',required=True)
    parser.add_argument('--public',action='store_true',help='Allow installations beyond the App owner')
    parser.add_argument('--allow-create',action='store_true',help='Backups only: request Administration write permission')
    parser.add_argument('--register',action='store_true',help='Start the operator registration flow (external action after confirmation on GitHub)')
    parser.add_argument('--secrets-dir',help='New directory outside Git, required with --register')
    parser.add_argument('--org')
    args=parser.parse_args();os.umask(0o077)
    value=manifest(args.kind,args.origin,args.name,args.allow_create,args.public)
    if args.register:
        if not args.secrets_dir:parser.error('--register requires --secrets-dir')
        register(args,value)
    else:
        print(json.dumps(value,indent=2))


if __name__=='__main__':
    main()
