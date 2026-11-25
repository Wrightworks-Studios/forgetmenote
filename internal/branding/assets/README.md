# ForgetMeNote identity

The approved mark is **Open bloom**: five separated, softly notched blue petals (`#3299d6`) around a gold center (`#f0c43f`). The gaps between petals keep the flower recognizable at small sizes. `flower.svg` is the single vector source used by the site header, sign-in page, modern browser favicon, and raster exports below. Earlier explorations in `docs/brand-study/` are historical references.

- `favicon.ico` contains transparent 16, 32, and 48 px versions for legacy browsers/bookmarks.
- `apple-touch-icon.png` is 180 px with a dark `#111b27` background.
- `app-icon-512.png` and `app-icon-192.png` are the matching application artwork; the web app manifest lists them for installed-app launchers.
- `app-icon-maskable-512.png` is the manifest's maskable icon: the same dark square with the flower at scale 5.3 from `(86.4,86.4)`, so the whole flower stays inside the central circle that launchers may crop to.

The PNG app icons place the 64-unit flower at `(40,40)` with scale 6.75 on a 512 px square. Rasterize with librsvg (`rsvg-convert`); the local ImageMagick SVG fallback mishandles rotated petal transforms. Export transparent favicon frames directly at 16, 32, and 48 px and package them with ImageMagick. Render the dark app-icon composition at 512, 192 and 180 px for the application and touch icons. All exports use the same flower geometry and colors.

Fraunces is used for the wordmark, welcome/empty-state heading, and sign-in/consent headings. Onest remains the interface font. Note-reading fonts and user settings are unchanged. Fraunces is bundled locally with its OFL license in `../fonts`.
