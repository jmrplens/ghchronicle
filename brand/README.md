# Brand

The mark is geometry, not a drawing: a five by five contribution grid whose
solid cells rise on the diagonal. That is why it lives as a generator rather
than as a folder of hand-drawn files. Changing the palette or the cell count is
one edit in `cmd/gen_brand` instead of twenty five in each of a dozen files.

Run from the root of the repository, writing into this directory:

```sh
go run ./cmd/gen_brand mark -out brand       # the mark and the favicon, per theme
go run ./cmd/gen_brand compose -out brand   # the banner, the social image and the og:image
```

`compose` reads the three `bg-*.png` backgrounds from the same directory it
writes to, and shells out to `rsvg-convert` for the rasters.

## Two greens, not one

GitHub's own green, and GitHub's own pair of values, for the reason GitHub has
two. Measured against this project's backgrounds:

| Colour | On the page (#0e1316) | On white |
|---|---|---|
| `#3fb950` | 7.36:1 | 2.54:1 |
| `#1a7f37` | 3.68:1 | 5.08:1 |

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

| File | What it is |
|---|---|
| `mark-dark.svg`, `mark-light.svg` | The mark, one per theme |
| `favicon-dark.svg`, `favicon-light.svg` | The three by three variant |
| `banner.svg` and `.png` | 1280x320, for the README |
| `social.svg` and `.png` | 1280x640, the repository social preview |
| `og.svg` and `.png` | 1200x630, the documentation `og:image` |
| `background.png` | The generated field the three compositions crop from |

The background was generated once with inference.sh (gpt-image-2) and is a
raster; everything drawn over it is vector, so the type stays crisp at whatever
size the raster is produced.

Setting the repository social preview is a manual step: Settings, then Social
preview, then upload `social.png`. GitHub offers no API for it.
