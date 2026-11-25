# Native Apple document adapter

The main Dockerfile builds `convert.cpp` against pinned Debian libetonyek
0.1.10-3+b1 and librevenge, then copies only the executable into the runtime.
The runtime installs the matching shared library; development compilers and
headers remain in the builder stage. Library licenses ship with Debian packages.

The library owns Pages/Keynote XML and IWA schema parsing. The adapter emits one
escaped JSON section for Pages or numbered physical slides for Keynote, including
blanks. It sets a 512 MiB address-space limit. Go preflights ZIP paths, expansion,
XML directives and Snappy blocks, then runs it through the bounded cancellation
boundary. Text extraction does not reproduce layout or structured table cells.

`make smoke-source` runs the real adapter in the final image on an internal
network whose only other container is the test database, against synthetic
legacy/modern fixtures. Their provenance and MPL 2.0 license
are in `internal/sources/testdata/iwork`. No runtime inference or downloads occur.
