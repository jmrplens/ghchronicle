# Brand

The mark is geometry, not a drawing: a five by five contribution grid whose
solid cells rise on the diagonal. That is why it lives as a generator rather
than as a folder of hand-drawn files. Changing the palette or the cell count is
one edit in `cmd/gen_brand` instead of twenty five in each of a dozen files.

Run from the root of the repository, writing into this directory:

```sh
go run ./cmd/gen_brand mark -out brand       # the mark and the favicon, per theme
go run ./cmd/gen_brand compose -out brand   # the banner, the social image and the og:image
go run ./cmd/gen_brand icons -out site/public   # what the documentation site serves
```

`compose` reads the three `bg-*.png` backgrounds from the same directory it
writes to, and shells out to `rsvg-convert` for the rasters.

`icons` writes straight into the site's public directory, and also shells out
to `rsvg-convert` (`make gen-brand-icons`):

| File in `site/public`          | What it is                                                                                                                             |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------- |
| `favicon.svg`                  | The three by three favicon for both themes in one file: the light green by default and the dark one under `prefers-color-scheme: dark` |
| `favicon.ico`                  | The same drawing at 16, 32 and 48 pixels, for clients that ask for `/favicon.ico` by name                                              |
| `apple-touch-icon.png`         | 180 pixels, the three by three drawing on the dark page, for an iOS home screen                                                        |
| `icon-192.png`, `icon-512.png` | The full mark on the dark page, the web manifest's home screen icons                                                                   |
| `icon-maskable-512.png`        | The same, with the grid small enough that its corners stay inside the circle of 80% of the side a platform may cut a maskable icon to  |
| `manifest.webmanifest`         | The web manifest that lists those icons                                                                                                |
| `og.png`                       | `brand/og.png` through `pngquant` when it is installed, about 190 KB instead of 500                                                    |

The home screen icons sit on an opaque ground on purpose: a transparent icon
disappears against the light and dark wallpapers a home screen actually has.
`cmd/gen_brand`'s tests read the committed files back and fail when one is not
the size it claims, when a home screen icon has a transparent corner, when the
maskable icon's ink leaves its safe zone, or when the favicon or the manifest is
not what `icons` would write now.

## Two greens, not one

GitHub's own green, and GitHub's own pair of values, for the reason GitHub has
two. Measured against this project's backgrounds:

| Colour    | On the page (#0e1316) | On white |
| --------- | --------------------- | -------- |
| `#3fb950` | 7.36:1                | 2.54:1   |
| `#1a7f37` | 3.68:1                | 5.08:1   |

Neither passes both. One colour per theme is not a refinement here, it is the
only way the mark and the links are legible in both. `#3fb950` is the dark
theme, `#1a7f37` the light one, and `site/scripts/check-contrast.mjs` fails
the site build if any pair drops below its WCAG 2.2 AA threshold.

The opacity ramp differs by theme for the same reason: 0.18 of a mid green on
white is indistinguishable from the page, which renders a five by five grid as
a three by three one.

## The favicon is a different drawing

Twenty five cells at sixteen pixels is mush, so the favicon drops to three by
three and keeps the diagonal, which is the part that carries the meaning.

## Files

| File                                    | What it is                                           |
| --------------------------------------- | ---------------------------------------------------- |
| `mark-dark.svg`, `mark-light.svg`       | The mark, one per theme                              |
| `favicon-dark.svg`, `favicon-light.svg` | The three by three variant                           |
| `banner.svg` and `.png`                 | 1280x320, for the README                             |
| `social.svg` and `.png`                 | 1280x640, the repository social preview              |
| `og.svg` and `.png`                     | 1200x630, the documentation `og:image`               |
| `background.png`                        | The generated field the three compositions crop from |

The background was generated once with inference.sh (gpt-image-2) and is a
raster; everything drawn over it is vector, so the type stays crisp at whatever
size the raster is produced.

Setting the repository social preview is a manual step: Settings, then Social
preview, then upload `social.png`. GitHub offers no API for it.
