# Synthetic source fixtures

`searchable`, `scanned`, `mixed`, `columns`, `table`, `equations`, `rotated` and
`poor` PDFs contain generated Amber/Cobalt evidence only. The OCR comparison
uses `scripts/ocr_benchmark.py`. The ruled table may yield no readable text; its test
requires a partial warning and retained original in that case.

Office fixtures use `generate.py`. Image fixtures contain synthetic English
text and a blank middle TIFF frame. `reference.msg` is a generated compound-file
email fixture; tests replace its body with synthetic HTML and compressed RTF.
The speech fixture is a synthetic announcement. None contains personal notes or
the supplied textbook. Apple fixtures have separate provenance/licensing in
`iwork/README.md` and `iwork/LICENSE`.

`release-a/` holds synthetic source job manifests, and one job's original and
result, written by the file-based release's own worker for the import-files
round trip.
