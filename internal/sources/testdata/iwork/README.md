These synthetic compatibility fixtures originate from the libetonyek 0.1.10
source archive's `src/test/data` directory, licensed under MPL 2.0 (LICENSE).
Upstream: https://dev-www.libreoffice.org/src/libetonyek/libetonyek-0.1.10.tar.xz

- modern.pages: pages5-file.pages; three multilingual example paragraphs.
- blank.key: keynote6-file.key; one blank modern slide.
- modern.key: keynote6-file.key, modified on 2026-10-09 to add the literal texts
  "Amber modern Keynote" and "Cobalt body evidence" to the existing slide's
  title/body storage objects. Only these IWA payloads and their lengths changed.
- legacy.key: keynote5-file.key; two slides with titles/body text.
- legacy.pages: pages4-file.pages; a blank legacy document.

They contain no author notebook or textbook content. Run the Go caller tests in
the runtime image with FMN_IWORK_TEST_FIXTURES pointing to this directory;
format tests require the actual `forgetmenote-iwork` executable.
