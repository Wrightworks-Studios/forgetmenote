"""Regenerate full, unsubstituted offline WOFF2 fonts with pinned build tooling."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / 'internal/web/ui/fonts'

def main():
    import fontTools
    import brotli
    from fontTools.ttLib import TTFont
    if fontTools.__version__ != '4.60.1' or brotli.__version__ != '1.1.0':
        raise SystemExit('Install scripts/font-requirements.txt in a private virtual environment.')
    records = []
    for path in sorted(DIRECTORY.glob('*.ttf')):
        target = path.with_suffix('.woff2')
        with TTFont(path, recalcTimestamp=False) as font:
            font.flavor = 'woff2'
            font.save(target)
            records.append({
                'source': path.name, 'compressed': target.name,
                'sourceSHA256': hashlib.sha256(path.read_bytes()).hexdigest(),
                'compressedSHA256': hashlib.sha256(target.read_bytes()).hexdigest(),
                'sourceBytes': path.stat().st_size, 'compressedBytes': target.stat().st_size,
                'glyphs': len(font.getGlyphOrder()),
            })
    # Verify the entire glyph inventory, variation/layout tables, and outlines
    # before recording the deliverable hashes. There is deliberately no subset.
    subprocess.run([sys.executable, '-m', 'unittest', 'scripts.test_fonts'], cwd=ROOT, check=True)
    (DIRECTORY / 'manifest.json').write_text(json.dumps({'version':1, 'fonts':records}, indent=2)+'\n')

if __name__ == '__main__':
    main()
