#!/usr/bin/env python3
"""Verify source archives in a disposable Docker instance (no personal project data)."""
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.error
import urllib.request

from smoke_app import app_port, container_token, wait_app
from smoke_database import Database


def pdf_bytes(text, stream=None):
    stream = stream or f"BT /F1 18 Tf 50 740 Td ({text}) Tj ET\n".encode()
    objects = [b"<< /Type /Catalog /Pages 2 0 R >>", b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
               b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
               b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
               b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"endstream"]
    data = b"%PDF-1.4\n"
    offsets = [0]
    for i, obj in enumerate(objects, 1):
        offsets.append(len(data))
        data += f"{i} 0 obj\n".encode() + obj + b"\nendobj\n"
    xref = len(data)
    data += f"xref\n0 {len(offsets)}\n0000000000 65535 f \n".encode()
    data += b"".join(f"{offset:010d} 00000 n \n".encode() for offset in offsets[1:])
    return data + f"trailer\n<< /Size {len(offsets)} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()


def table_pdf_bytes():
    lines = []
    for size, x, y, text in [(24, 50, 742, 'Inventory report'), (12, 50, 704, 'Stock levels for the current quarter.'),
                              (16, 65, 628, 'Item'), (16, 215, 628, 'Quantity'),
                              (16, 65, 593, 'Amber'), (16, 215, 593, '17'),
                              (16, 65, 558, 'Cobalt'), (16, 215, 558, '29')]:
        lines.append(f'BT /F1 {size} Tf {x} {y} Td ({text}) Tj ET')
    for y in [650, 615, 580, 545]:
        lines.append(f'50 {y} m 350 {y} l S')
    for x in [50, 200, 350]:
        lines.append(f'{x} 545 m {x} 650 l S')
    return pdf_bytes('', ('\n'.join(lines) + '\n').encode())


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def open_to_container(path):
    """Let the image's own user read a disposable bind mount.

    The release gate runs under umask 077 and containers run as another user.
    Docker Desktop hides that difference; a Linux host enforces it.
    """
    paths = [path, *path.rglob('*')] if path.is_dir() else [path]
    for item in paths:
        item.chmod(0o755 if item.is_dir() or item.stat().st_mode & 0o100 else 0o644)


def every_integration(image):
    """The release image is the default build, which includes every optional
    integration (docs/integrations.md)."""
    listed = subprocess.run(['docker', 'run', '--rm', '--platform', 'linux/amd64', '--network', 'none', '--read-only',
                             image, 'integrations'], check=True, capture_output=True, text=True).stdout
    states = [line.split('\t')[1] for line in listed.splitlines()]
    assert states and set(states) == {'included'}, listed


def offline_pdf_fixtures(image, *, full=True):
    """Exercise real native parsers in the final runtime, without networking.

    Build a disposable Go test executable; the compiler stays on the host. The
    full suite includes all format families, hostile input and local
    checkpoint/restart. The image's one document tool is Poppler's pdftotext.
    """
    repo = Path(__file__).resolve().parents[1]
    fixtures = repo / 'internal/sources/testdata'
    packaging = r"""
import importlib.util, pathlib, shutil
for name in ('docling', 'torch', 'torchvision', 'transformers', 'huggingface_hub'):
    assert importlib.util.find_spec(name) is None, 'displaced inference dependency: ' + name
assert not pathlib.Path('/usr/local/bin/forgetmenote-convert').exists()
assert not pathlib.Path('/opt/docling').exists()
assert shutil.which('pdftotext'), 'Poppler pdftotext is missing'
# Connected agents read scans, images, media and Apple documents themselves.
for name in ('tesseract', 'ffmpeg', 'ffprobe', 'forgetmenote-iwork'):
    assert shutil.which(name) is None, 'removed extractor present: ' + name
assert not pathlib.Path('/usr/share/tesseract-ocr').exists()
assert pathlib.Path('/usr/share/forgetmenote/source-versions').read_text().startswith('Poppler ')
"""
    subprocess.run(['docker', 'run', '--rm', '--platform', 'linux/amd64', '--network', 'none', '--read-only',
                    '--entrypoint', 'python', image, '-c', packaging], check=True)
    with tempfile.TemporaryDirectory(prefix='fmn-source-check-') as temporary:
        executable = Path(temporary) / 'sources.test'
        subprocess.run(['go', 'test', '-c', '-o', str(executable), './internal/sources'],
                       cwd=repo, env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
        mounted = Path(temporary) / 'fixtures'
        shutil.copytree(repo / 'internal/sources', mounted)
        open_to_container(executable)
        open_to_container(mounted)
        selection = '.' if full else '^TestNativePDFReadsTheTextLayerOnly$/scanned'
        # Source job tests keep their manifests in Postgres. The database is the
        # only other container on an internal network, so the matrix reaches
        # nothing else.
        db = Database(internal=True) if full else None
        network = ['--network', db.network, '-e', 'FMN_TEST_DATABASE_URL=' + db.url()] if db else ['--network', 'none']
        try:
            result = subprocess.run(['docker', 'run', '--rm', '--init', '--platform', 'linux/amd64', *network,
                                     '--memory', '1g', '--cpus', '2', '--read-only', '--tmpfs', '/tmp:size=128m,exec',
                                     '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                                     '--mount', f'type=bind,source={executable},target=/checks/sources.test,readonly',
                                     '--mount', f'type=bind,source={mounted},target=/fixtures,readonly',
                                     '-w', '/fixtures',
                                     '--entrypoint', '/checks/sources.test', image,
                                     '-test.run', selection, '-test.timeout=15m', '-test.v'],
                                    capture_output=True, text=True, timeout=960)
        finally:
            if db:
                db.close()
        assert result.returncode == 0, result.stdout + result.stderr
        assert '--- SKIP: TestNativePDFReadsTheTextLayerOnly' not in result.stdout, result.stdout
        print('PASS: network-isolated native source ' + ('complete format matrix' if full else 'scanned PDF fixture'))
    return [(label, (fixtures / (label + '.pdf')).read_bytes()) for label in (('scanned', 'mixed', 'searchable') if full else ('scanned',))]


def main():
    image = os.environ.get('SMOKE_IMAGE', 'forgetmenote:sources-test')
    profile = os.environ.get('SMOKE_PROFILE', 'full')
    if profile not in ('full', 'routine'):
        raise ValueError('SMOKE_PROFILE must be full or routine')
    full = profile == 'full'
    every_integration(image)
    pdf_fixtures = offline_pdf_fixtures(image, full=full)
    name = f'fmn-source-smoke-{os.getpid()}'
    volume = name + '-data'
    port = app_port()
    origin = f'http://localhost:{port}'
    run('docker', 'volume', 'create', volume)
    db = Database()
    token = ''

    def request(path, body=None):
        req = urllib.request.Request(origin + '/p/default' + path,
                                     data=json.dumps(body).encode() if body is not None else None,
                                     headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(req, timeout=120) as response:
            return json.load(response)

    def ready():
        return wait_app(origin, container_token(name))

    try:
        db.migrate(image)
        run('docker', 'run', '--init', '-d', '--name', name, '--read-only', '--tmpfs', '/tmp:size=64m',
            '-p', f'127.0.0.1:{port}:8080', '-e', f'PUBLIC_URL={origin}', '-v', volume + ':/data', *db.args(), image)
        token = ready()
        original = pdf_bytes('Rare amber paradigm')
        prepared = request('/api/sources/preview', {'filename': 'grammar.pdf', 'data': base64.b64encode(original).decode()})
        assert prepared['capture']['record']['extraction'] == 'extracted', prepared['capture']['record']['warning']
        assert 'Poppler' in prepared['capture']['record']['extractor'] and 'Tesseract' not in prepared['capture']['record']['extractor'], prepared['capture']['record']
        assert '## Page 1' in prepared['preview'] and 'Rare amber paradigm' in prepared['preview']
        first = request('/api/sources/import', {'prepared': prepared, 'title': 'Grammar evidence'})
        assert 'checksum matches' in request('/api/sources/verify?path=' + first['path'])['originalState']
        assert any(note['path'] == first['path'] for note in request('/api/notes?q=amber'))
        updated = pdf_bytes('New cobalt paradigm')
        prepared = request('/api/sources/preview', {'filename': 'grammar.pdf', 'data': base64.b64encode(updated).decode()})
        second = request('/api/sources/import', {'prepared': prepared, 'title': 'Grammar evidence', 'path': first['path'], 'revision': first['revision']})
        assert first['metadata']['source']['id'] == second['metadata']['source']['id']
        old = request('/api/note?path=' + first['path'] + '&revision=' + first['revision'])
        assert 'Rare amber paradigm' in old['content']
        # HTML capture remains inert, and failure to extract PDF text is explicit.
        html = b'<html><title>Botanical source</title><body><h1>Evidence</h1><p>Forget-me-not flower fixture.</p><script>neverRun()</script></body></html>'
        prepared = request('/api/sources/preview', {'filename': 'flower.html', 'data': base64.b64encode(html).decode(), 'url': 'https://example.org/fixture'})
        assert 'neverRun()' not in prepared['preview']
        webpage = request('/api/sources/import', {'prepared': prepared, 'title': 'Botanical source'})
        invalid = request('/api/sources/preview', {'filename': 'broken.pdf', 'data': base64.b64encode(b'%PDF-1.4\nbroken').decode()})
        assert invalid['capture']['record']['extraction'] == 'failed'
        try:
            request('/api/sources/preview', {'url': 'http://169.254.169.254/latest/meta-data'})
            raise AssertionError('Metadata URL accepted')
        except urllib.error.HTTPError as error:
            assert error.code >= 400
        if os.environ.get('SOURCE_SMOKE_URL'):
            prepared = request('/api/sources/preview', {'url': os.environ['SOURCE_SMOKE_URL']})
            assert prepared['capture']['record']['method'] == 'server-http'
            assert prepared['capture']['record']['finalURL']
            request('/api/sources/import', {'prepared': prepared, 'title': 'Public webpage test'})
            print('PASS: public URL capture')
        documents, awaiting = [], []
        for label, data in pdf_fixtures:
            prepared = request('/api/sources/preview', {'filename': label + '.pdf', 'data': base64.b64encode(data).decode()})
            record = prepared['capture']['record']
            assert 'Verify against the retained original' in record['warning'], record
            if label == 'scanned':
                # No text layer: the pages wait for a connected agent.
                assert record['extraction'] == 'no-text' and 'connected agent' in record['warning'], record
                assert 'id="h-page-3"' in prepared['html'] and '## Page 2' in prepared['preview']
            else:
                assert record['extraction'] == 'extracted', record
                assert ('Cobalt' if label == 'searchable' else 'Native header') in prepared['preview']
            note = request('/api/sources/import', {'prepared': prepared, 'title': label + ' evidence'})
            if label == 'searchable':
                assert any(found['path'] == note['path'] for found in request('/api/notes?q=Cobalt'))
            if label == 'scanned':
                awaiting.append(note['path'])
            documents.append((note, data))
        formats = request('/api/sources/formats')
        assert any('.xlsx' in item['extensions'] for item in formats)
        fixtures = Path(__file__).resolve().parents[1] / 'internal/sources/testdata'
        for ext in (['txt', 'csv', 'tsv', 'docx', 'epub', 'xlsx'] if full else []):
            data = (fixtures / ('reference.' + ext)).read_bytes()
            prepared = request('/api/sources/preview', {'filename': 'reference.' + ext, 'data': base64.b64encode(data).decode()})
            assert 'Amber' in prepared['capture']['text']
            options = {'folder': 'research/references'} if ext == 'txt' else {'folder': ''} if ext == 'csv' else {}
            note = request('/api/sources/import', {'prepared': prepared, **options})
            if ext == 'txt': assert note['path'].startswith('research/references/')
            if ext == 'csv': assert '/' not in note['path']
            assert any(found['path'] == note['path'] for found in request('/api/notes?q=Amber'))
            assert request('/api/sources/verify?path=' + note['path'])['originalState'] == 'Original checksum matches'
            req = urllib.request.Request(origin + '/p/default/assets/' + note['metadata']['source']['original'] + '?revision=' + note['revision'], headers={'Authorization': 'Bearer ' + token})
            with urllib.request.urlopen(req) as response:
                assert response.headers['Content-Type'] == note['metadata']['source']['mime']
                assert response.headers['Content-Disposition'].startswith('attachment;')
                assert response.read() == data
            documents.append((note, data))
        for filename, data, evidence in ([
            ('message.eml', b'From: Alice <alice@example.test>\nSubject: Amber meeting\n\nCobalt email evidence.', 'Cobalt'),
        ] if full else []):
            prepared = request('/api/sources/preview', {'filename': filename, 'data': base64.b64encode(data).decode()})
            assert evidence.lower() in prepared['capture']['text'].lower(), prepared
            assert prepared['capture']['record']['extraction'] == 'extracted', prepared
            note = request('/api/sources/import', {'prepared': prepared})
            assert any(found['path'] == note['path'] for found in request('/api/notes?q=' + evidence))
            req = urllib.request.Request(origin + '/p/default/assets/' + note['metadata']['source']['original'] + '?revision=' + note['revision'], headers={'Authorization': 'Bearer ' + token})
            with urllib.request.urlopen(req) as response:
                assert response.headers['Content-Disposition'].startswith('attachment;')
                assert response.read() == data
            documents.append((note, data))
        # The server stores agent formats as their originals alone.
        for filename in (['announcement.wav', 'reference.png', 'iwork/modern.pages'] if full else []):
            data = (fixtures / filename).read_bytes()
            prepared = request('/api/sources/preview', {'filename': Path(filename).name, 'data': base64.b64encode(data).decode()})
            record = prepared['capture']['record']
            assert record['extraction'] == 'original-only' and 'connected agent' in record['warning'], record
            note = request('/api/sources/import', {'prepared': prepared})
            awaiting.append(note['path'])
            documents.append((note, data))
        listed = {source['path'] for source in request('/api/sources/awaiting-text')['sources']}
        assert set(awaiting) <= listed, (awaiting, listed)
        run('docker', 'restart', name)
        ready()
        assert request('/api/note?path=' + first['path'])['content'] == second['content']
        assert request('/api/sources/verify?path=' + first['path'])['textState'] == 'Extracted text checksum matches'
        with tempfile.TemporaryDirectory() as tmp:
            req = urllib.request.Request(origin + '/p/default/api/backup', headers={'Authorization': 'Bearer ' + token})
            bundle = Path(tmp) / 'source.bundle'
            with urllib.request.urlopen(req) as response:
                bundle.write_bytes(response.read())
            clone = Path(tmp) / 'clone'
            run('git', 'clone', str(bundle), str(clone))
            assert (clone / first['metadata']['source']['original']).read_bytes() == original
            assert (clone / second['metadata']['source']['original']).read_bytes() == updated
            assert (clone / webpage['metadata']['source']['original']).read_bytes() == html
            for note, data in documents:
                assert (clone / note['metadata']['source']['original']).read_bytes() == data
                assert request('/api/note?path=' + note['path'])['content'] == note['content']
        print('PASS: ' + ('complete document extraction matrix' if full else 'PDF text and HTML capture') + ', sources awaiting agent text, search, capture history, restart and independent Git restoration')
        if os.environ.get('SOURCE_SMOKE_KEEP'):
            print(json.dumps({'container': name, 'volume': volume, 'database': db.name, 'network': db.network, 'origin': origin, 'note': second['path']}))
    finally:
        if not os.environ.get('SOURCE_SMOKE_KEEP'):
            subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'volume', 'rm', volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            db.close()


if __name__ == '__main__':
    main()
