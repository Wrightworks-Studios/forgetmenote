# Synthetic source fixtures

`searchable`, `scanned`, `mixed`, `columns`, `table`, `equations`, `rotated` and
`poor` PDFs contain generated Amber/Cobalt evidence only. Only `searchable` and
`mixed` have a text layer; the others are scans, which wait for a connected agent.

Office fixtures use `generate.py`. Image fixtures contain synthetic English
text and a blank middle TIFF frame. `reference.msg` is a generated compound-file
email fixture; tests replace its body with synthetic HTML and compressed RTF.
The speech fixture is a synthetic announcement. None contains personal notes or
the supplied textbook. Image, speech and Apple fixtures are stored as originals
only. Apple fixtures have separate provenance/licensing in
`iwork/README.md` and `iwork/LICENSE`.

`release-a/` holds synthetic source job manifests, and one job's original and
result, written by the file-based release's own worker for the import-files
round trip.
