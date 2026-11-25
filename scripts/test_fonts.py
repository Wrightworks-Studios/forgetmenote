"""Full-glyph offline font delivery contract; tooling is build-only."""
import unittest
from pathlib import Path
from fontTools.ttLib import TTFont
from fontTools.pens.recordingPen import RecordingPen

ROOT = Path(__file__).resolve().parents[1] / 'internal/web/ui/fonts'

class FontsTest(unittest.TestCase):
    def test_compressed_fonts_preserve_glyphs_variations_and_layout(self):
        for path in sorted(ROOT.glob('*.ttf')):
            with self.subTest(font=path.name):
                target = path.with_suffix('.woff2')
                self.assertTrue(target.is_file(), f'missing compressed font: {target.name}')
                self.assertLess(target.stat().st_size, path.stat().st_size)
                with TTFont(path) as original, TTFont(target) as compressed:
                    self.assertEqual(original.getGlyphOrder(), compressed.getGlyphOrder())
                    self.assertEqual(original['cmap'].compile(original), compressed['cmap'].compile(compressed))
                    for tag in ['fvar', 'gvar', 'GSUB', 'GPOS', 'GDEF', 'name', 'hmtx', 'OS/2']:
                        if tag in original:
                            self.assertEqual(original[tag].compile(original), compressed[tag].compile(compressed), tag)
                    old, new = original.getGlyphSet(), compressed.getGlyphSet()
                    for name in original.getGlyphOrder():
                        a, b = RecordingPen(), RecordingPen()
                        old[name].draw(a); new[name].draw(b)
                        self.assertEqual(a.value, b.value, name)
                        self.assertEqual(old[name].width, new[name].width, name)

if __name__ == '__main__':
    unittest.main()
