#!/usr/bin/env python3
"""Local-only OCR comparison; development libraries never enter the runtime image.

Use the bundled Python with Pillow, reportlab and pypdf. Results, traineddata,
and any supplied book subsets belong in a private directory outside the repo.
No document content is sent over the network; only pinned public OCR data is
downloaded. Existing text is a comparison reference, not OCR ground truth.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.request
import uuid

from PIL import Image, ImageFilter
from pypdf import PdfReader, PdfWriter
from reportlab.pdfgen import canvas


MODELS = {
    'fast': '87416418657359cb625c412a48b6e1d6d41c29bd',
    'best': 'e12c65a915945e4c28e237a9b52bc4a8f39a0cec',
}
MODEL_HASHES = {
    'fast': '7d4322bd2a7749724879683fc3912cb542f19906c83bcc1a52132556427170b2',
    'best': '8280aed0782fe27257a68ea10fe7ef324ca0f8d85bd2fd145d1c2b560bcb66ba',
}
PROSE = ('Amber evidence remains searchable after capture. '
         'Cobalt findings retain the original physical page number. '
         'Local processing never uploads this document to an external service.')


def words(text):
    return re.findall(r'\w+', text.lower())


def word_error(reference, actual):
    reference, actual = words(reference), words(actual)
    previous = list(range(len(actual) + 1))
    for i, want in enumerate(reference, 1):
        current = [i]
        for j, got in enumerate(actual, 1):
            current.append(min(current[-1] + 1, previous[j] + 1,
                               previous[j - 1] + (want != got)))
        previous = current
    return round(previous[-1] / max(1, len(reference)), 4)


def text_pdf(lines, *, columns=False, table=False, rotation=0):
    output = io.BytesIO()
    page = canvas.Canvas(output, pagesize=(612, 792), pageCompression=0)
    page.setFont('Helvetica', 14)
    for i, text in enumerate(lines):
        page.drawString(50 if not columns or i % 2 == 0 else 330,
                        735 - (i if not columns else i // 2) * 30, text)
    if table:
        for y in (745, 715, 685, 655):
            page.line(45, y, 360, y)
        for x in (45, 200, 360):
            page.line(x, 655, x, 745)
    page.save()
    if rotation:
        reader = PdfReader(io.BytesIO(output.getvalue()))
        writer = PdfWriter()
        writer.add_page(reader.pages[0].rotate(rotation))
        output = io.BytesIO()
        writer.write(output)
    return output.getvalue()


def command(image, root, *args, timeout=120):
    started = time.monotonic()
    container = 'fmn-ocr-evaluation-' + uuid.uuid4().hex
    runner = r'''
import json, resource, subprocess, sys, time
args=json.loads(sys.argv[1]); started=time.monotonic()
p=subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
usage=resource.getrusage(resource.RUSAGE_CHILDREN)
sys.stdout.buffer.write(json.dumps({'seconds':round(time.monotonic()-started,4),
 'cpuSeconds':round(usage.ru_utime+usage.ru_stime,4), 'peakMiB':round(usage.ru_maxrss/1024,2),
 'exit':p.returncode}).encode()+b'\n'+p.stdout)
'''
    cmd = ['docker', 'run', '--rm', '--name', container, '--platform', 'linux/amd64', '--network', 'none',
           '--memory', '2g', '--cpus', '2', '--read-only', '--tmpfs', '/tmp:size=256m',
           '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
           '-e', 'OMP_THREAD_LIMIT=1', '-e', 'OMP_NUM_THREADS=1',
           '--mount', f'type=bind,source={root},target=/evaluation',
           '--entrypoint', 'python', image, '-c', runner, json.dumps(args)]
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    except subprocess.TimeoutExpired:
        subprocess.run(['docker', 'rm', '-f', container], capture_output=True, check=True)
        return b'', {'exit': 124, 'seconds': round(time.monotonic() - started, 4),
                     'deadlineExceeded': True}
    if p.returncode:
        raise RuntimeError('evaluation container failed: ' + p.stderr.decode()[-1000:])
    header, _, output = p.stdout.partition(b'\n')
    metrics = json.loads(header)
    metrics['containerSeconds'] = round(time.monotonic() - started, 4)
    return output, metrics


def render(image, root, source, page, destination):
    _, metrics = command(image, root, 'pdftoppm', '-f', str(page), '-l', str(page),
                         '-r', '300', '-scale-to', '4096', '-singlefile', '-png',
                         '/evaluation/' + source, '/evaluation/' + destination)
    if metrics['exit']:
        raise RuntimeError('fixture rendering failed')
    return root / (destination + '.png')


def fixtures(image, root, book):
    specs = {
        'searchable': [PROSE[:46], PROSE[47:119], PROSE[120:]],
        'columns': ['Amber left column', 'Cobalt right column',
                    'First evidence paragraph', 'Second evidence paragraph'],
        'table': ['Item                 Count', 'Amber              17', 'Cobalt              29'],
        'equations': ['E = mc^2', 'f(x) = x^2 + 2x + 1', 'sum(n = 1..10) = 55'],
        'rotated': ['Amber rotated page', 'Cobalt rotation evidence'],
    }
    cases = []
    for name, lines in specs.items():
        original = root / (name + '-native.pdf')
        original.write_bytes(text_pdf(lines, columns=name == 'columns',
                                      table=name == 'table', rotation=90 if name == 'rotated' else 0))
        path = original
        if name != 'searchable':
            png = render(image, root, original.name, 1, name)
            path = root / (name + '.pdf')
            with Image.open(png) as scan:
                scan.convert('RGB').save(path, 'PDF', resolution=300)
        cases.append((name, path, '\n'.join(lines), 'synthetic ground truth'))
    prose = render(image, root, 'searchable-native.pdf', 1, 'scanned')
    with Image.open(prose) as scan:
        clean = scan.convert('RGB')
        blank = Image.new('RGB', clean.size, 'white')
        clean.save(root / 'scanned.pdf', 'PDF', resolution=300, save_all=True,
                   append_images=[blank, clean])
        poor = clean.resize((clean.width // 4, clean.height // 4)).filter(ImageFilter.GaussianBlur(.7))
        poor.save(root / 'poor.pdf', 'PDF', resolution=75)
        # Mixed within ONE page: native words above a scan with different words.
        crop = clean.crop((0, 0, clean.width, 650))
        crop.save(root / 'mixed-region.png')
    mixed = io.BytesIO()
    page = canvas.Canvas(mixed, pagesize=(612, 792))
    page.drawString(50, 760, 'Native header is retained exactly once.')
    page.drawImage(str(root / 'mixed-region.png'), 0, 550, width=612, height=156)
    page.save()
    (root / 'mixed.pdf').write_bytes(mixed.getvalue())
    cases += [('scanned', root / 'scanned.pdf', '\n'.join(specs['searchable'] * 2), 'synthetic ground truth'),
              ('poor', root / 'poor.pdf', '\n'.join(specs['searchable']), 'synthetic ground truth'),
              ('mixed', root / 'mixed.pdf', 'Native header is retained exactly once.\n' + '\n'.join(specs['searchable']), 'synthetic ground truth')]
    if book:
        reader, writer = PdfReader(book), PdfWriter()
        selected = sorted({min(n, len(reader.pages)) for n in (6, 15, 30, 60, 100, 140, 180, 220, 270)})
        for number in selected:
            writer.add_page(reader.pages[number - 1])
        with (root / 'book-sample.pdf').open('wb') as output:
            writer.write(output)
        (root / 'book-sample-pages.json').write_text(json.dumps(selected))
        cases.append(('book-sample', root / 'book-sample.pdf',
                      '\n'.join(p.extract_text() or '' for p in writer.pages), 'existing text-layer agreement'))
    return cases


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--book', type=Path)
    parser.add_argument('--baseline', action='store_true', help='also measure the old Docling wrapper')
    parser.add_argument('--only', nargs='+', help='measure only named cases after investigating a fixture change')
    args = parser.parse_args()
    os.umask(0o077)
    root = args.output.resolve()
    repo = Path(__file__).resolve().parents[1]
    if root == repo or repo in root.parents:
        parser.error('evaluation output must be outside the source repository')
    root.mkdir(parents=True, exist_ok=True)
    models = []
    for name, revision in MODELS.items():
        directory = root / name
        directory.mkdir(exist_ok=True)
        path = directory / 'eng.traineddata'
        if not path.exists():
            url = f'https://raw.githubusercontent.com/tesseract-ocr/tessdata_{name}/{revision}/eng.traineddata'
            with urllib.request.urlopen(url, timeout=30) as response:
                data = response.read(32 << 20)
                if response.read(1):
                    raise RuntimeError('traineddata download exceeds its bound')
            path.write_bytes(data)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        if digest != MODEL_HASHES[name]:
            raise RuntimeError('traineddata checksum mismatch: ' + name)
        models.append({'name': name, 'revision': revision, 'bytes': path.stat().st_size,
                       'sha256': digest})
    cases = fixtures(args.image, root, args.book)
    report = {'image': args.image, 'models': models, 'cases': []}
    if args.only and (root / 'results.json').exists():
        prior = json.loads((root / 'results.json').read_text())
        if prior['image'] != args.image or prior['models'] != models:
            parser.error('existing measurements use different engine or model inputs')
        report['cases'] = [r for r in prior['cases'] if r['name'] not in args.only]
    for name, path, reference, reference_kind in cases:
        if args.only and name not in args.only:
            continue
        text_layer, native = command(args.image, root, 'pdftotext', '-layout', str('/evaluation/' + path.name), '-')
        row = {'name': name, 'pages': len(PdfReader(path).pages), 'reference': reference_kind,
               'textLayer': {**native, 'bytes': len(text_layer)}}
        if args.baseline:
            data, metrics = command(args.image, root, 'forgetmenote-convert', '/evaluation/' + path.name, timeout=120)
            if metrics['exit'] == 0:
                result = json.loads(data)
                extracted = '\n'.join(b.get('text', '') or ' '.join(c for r in b.get('rows', []) for c in r)
                                      for s in result['sections'] for b in s['blocks'])
                metrics.update(wordError=word_error(reference, extracted), partial=result['partial'])
            row['docling'] = metrics
        rendered = [render(args.image, root, path.name, page, name + f'-page-{page}')
                    for page in range(1, row['pages'] + 1)]
        for model in MODELS:
            total, cpu, peak, extracted = 0, 0, 0, []
            for png in rendered:
                data, metrics = command(args.image, root, 'tesseract', '/evaluation/' + png.name, 'stdout',
                                        '--tessdata-dir', '/evaluation/' + model, '-l', 'eng', '--psm', '3')
                if metrics['exit']:
                    raise RuntimeError('Tesseract failed on ' + name)
                total += metrics['seconds']
                cpu += metrics['cpuSeconds']
                peak = max(peak, metrics['peakMiB'])
                extracted.append(data.decode('utf-8'))
            row[model] = {'seconds': round(total, 4), 'cpuSeconds': round(cpu, 4), 'peakMiB': peak,
                          'wordError': word_error(reference, '\n'.join(extracted))}
        report['cases'].append(row)
        (root / 'results.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(row), flush=True)


if __name__ == '__main__':
    main()
