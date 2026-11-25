"""An in-memory S3 stand-in for container smokes.

It serves one bucket path-style and answers only requests signed with Signature
Version 4 under its access key over the payload they carry, as S3 does, so a
gateway reaches it through its ordinary S3 client and the AWS credential chain.
Lists page two keys at a time, so clients must follow continuation tokens. Each
request is logged as its method, path and status; keys carry no secrets.

Usage: python3 -I smoke_s3.py PORT BUCKET REGION, with SMOKE_S3_ACCESS_KEY and
SMOKE_S3_SECRET_KEY in the environment.
"""
import hashlib
import hmac
import os
import sys
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape

PORT, BUCKET, REGION = int(sys.argv[1]), sys.argv[2], sys.argv[3]
ACCESS, SECRET = os.environ['SMOKE_S3_ACCESS_KEY'], os.environ['SMOKE_S3_SECRET_KEY']
objects, lock = {}, threading.Lock()


def quote(value):
    return urllib.parse.quote(value, safe='-_.~')


def signed(handler, path, query, body):
    payload = handler.headers.get('X-Amz-Content-Sha256', '')
    if payload != hashlib.sha256(body).hexdigest():
        return False
    scheme, _, fields = handler.headers.get('Authorization', '').partition(' ')
    parts = dict(field.split('=', 1) for field in fields.split(', ') if '=' in field)
    date = handler.headers.get('X-Amz-Date', '')
    scope = f'{date[:8]}/{REGION}/s3/aws4_request'
    if scheme != 'AWS4-HMAC-SHA256' or parts.get('Credential') != f'{ACCESS}/{scope}':
        return False
    names = parts.get('SignedHeaders', '').split(';')
    if 'host' not in names or 'x-amz-date' not in names:
        return False
    headers = ''.join(f"{name}:{' '.join(handler.headers.get(name, '').split())}\n" for name in names)
    pairs = sorted((quote(k), quote(v)) for k, v in urllib.parse.parse_qsl(query, keep_blank_values=True))
    canonical = '\n'.join([handler.command, path, '&'.join(f'{k}={v}' for k, v in pairs), headers, ';'.join(names), payload])
    text = '\n'.join(['AWS4-HMAC-SHA256', date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ('AWS4' + SECRET).encode()
    for part in (date[:8], REGION, 's3', 'aws4_request'):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    return hmac.compare_digest(parts.get('Signature', ''), hmac.new(key, text.encode(), hashlib.sha256).hexdigest())


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, format, *args):
        print(format % args, flush=True)

    def reply(self, status, body=b'', headers=()):
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(body)

    def fault(self, status, code):
        self.reply(status, f'<Error><Code>{code}</Code><Message>stand-in</Message></Error>'.encode(), [('Content-Type', 'application/xml')])

    def handle_one(self):
        path, _, query = self.path.partition('?')
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        if not signed(self, path, query, body):
            return self.fault(403, 'SignatureDoesNotMatch')
        if path != '/' + BUCKET and not path.startswith('/' + BUCKET + '/'):
            return self.fault(404, 'NoSuchBucket')
        key = urllib.parse.unquote(path[len(BUCKET) + 2:])
        params = dict(urllib.parse.parse_qsl(query, keep_blank_values=True))
        with lock:
            if not key and self.command == 'GET' and params.get('list-type') == '2':
                return self.list(params.get('prefix', ''), params.get('continuation-token', ''))
            if not key:
                return self.fault(405, 'MethodNotAllowed')
            if self.command == 'PUT':
                objects[key] = body
                return self.reply(200)
            if self.command == 'DELETE':
                objects.pop(key, None)
                return self.reply(204)
            if self.command != 'GET':
                return self.fault(405, 'MethodNotAllowed')
            if key not in objects:
                return self.fault(404, 'NoSuchKey')
            data = objects[key]
        spec = self.headers.get('Range')
        if not spec:
            return self.reply(200, data)
        first, _, last = spec.removeprefix('bytes=').partition('-')
        first, last = int(first), min(int(last), len(data) - 1)
        if first > last:
            return self.fault(416, 'InvalidRange')
        self.reply(206, data[first:last + 1], [('Content-Range', f'bytes {first}-{last}/{len(data)}')])

    def list(self, prefix, after):
        keys = sorted(key for key in objects if key.startswith(prefix) and key > after)
        page, rest = keys[:2], keys[2:]
        xml = ''.join(f'<Contents><Key>{escape(key)}</Key></Contents>' for key in page)
        more = f'<IsTruncated>true</IsTruncated><NextContinuationToken>{escape(page[-1])}</NextContinuationToken>' if rest else '<IsTruncated>false</IsTruncated>'
        self.reply(200, f'<ListBucketResult>{xml}{more}</ListBucketResult>'.encode(), [('Content-Type', 'application/xml')])

    do_GET = do_PUT = do_DELETE = do_HEAD = do_POST = handle_one


ThreadingHTTPServer(('0.0.0.0', PORT), Handler).serve_forever()
