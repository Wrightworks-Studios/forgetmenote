# Onest

Variable font, weights 100–900, bundled for offline use.
Source: https://github.com/google/fonts/tree/4c1db3aec83c67dd223dd82a68c039cab30917a9/ofl/onest
Original filename: `Onest[wght].ttf` (unmodified bytes).
License: SIL Open Font License 1.1; see OFL.txt.
SHA-256: `966c5c29b4755da84b6854d5c21dd4eaa2420225d0e9874de602de176d4a9f31`

# Source Serif 4

Adobe variable serif, regular and italic, weights 200–900 and optical sizes 8–60.
Bundled unmodified for offline reading; SIL Open Font License 1.1.
Source: https://github.com/adobe-fonts/source-serif/tree/5f220b17d27ed64873f22cde0dd593685387bd19/VAR
License: SourceSerif4-LICENSE.md

SHA-256:
- `SourceSerif4-Roman.ttf`: `14d360ee1b76655da9276628b229e11671bc1f5d1083636144db6677d452cf55`
- `SourceSerif4-Italic.ttf`: `6a059a64838978d54e8fab71ed86b0d82e948c0e12b2664d0c15166326dcff82`

# Fraunces

Variable display serif, bundled unmodified for the ForgetMeNote wordmark and welcome/sign-in headings. UI controls remain Onest; note typography remains user-configurable.
Source: https://github.com/google/fonts/tree/main/ofl/fraunces (downloaded 2026-09-07).
Original filename: `Fraunces[SOFT,WONK,opsz,wght].ttf`.
License: SIL Open Font License 1.1; see Fraunces-OFL.txt.
SHA-256: `177ff6c0f14e5550a3c624247cd1189611d4eb65d000b14944c63d967958abbb`.

# Compressed delivery

The UI loads fingerprinted WOFF2 versions of all four fonts from the build.
No glyphs are subsetted: 4,131 glyphs, variable axes, outlines, character maps
and layout tables are retained. The original TTF files are the regeneration
sources only: the server embeds and serves neither them nor these WOFF2 copies,
only the license texts in this directory.

Regenerate only in a private virtual environment with
`scripts/font-requirements.txt`, then run `python scripts/fonts.py`. This checks
all glyph outlines and widths and the full cmap, variation and layout tables
before writing `manifest.json`. Every ordinary npm build verifies both source
and compressed hashes without requiring Python or font tools.

The four files total 2,607,740 TTF bytes and 1,052,720 WOFF2 bytes (59.6% less).
Fraunces and Onest, loaded on ordinary pages, total 553,496 TTF bytes and
278,992 WOFF2 bytes (49.6% less).
