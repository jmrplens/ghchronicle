// Command gen_brand writes the ghchronicle mark, and the compositions built on
// it.
//
// The mark is geometry rather than a drawing: a five by five contribution grid
// whose solid cells rise on the diagonal. One generator rather than a folder of
// hand-drawn files, because changing the palette or the cell count is then one
// edit here instead of twenty five in each of a dozen files.
//
// Two families of files come out of that geometry, and each is a subcommand so
// the two stay distinguishable:
//
//	go run ./cmd/gen_brand mark       # the mark and the favicon, per theme
//	go run ./cmd/gen_brand compose    # the banner, the social image and the og:image
//	go run ./cmd/gen_brand icons -out site/public  # what the site serves to name itself
//	go run ./cmd/gen_brand mark -out brand
//
// The mark family is pure text. The compose family reads a background raster
// out of the same directory, embeds it, and shells out to rsvg-convert for the
// PNG that actually ships.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// GitHub's own green, and its own pair of values, for the reason GitHub has
// two: measured against this project's backgrounds, #3fb950 scores 7.36:1 on
// the near-black page and 2.54:1 on white, and #1a7f37 scores 5.08:1 on white
// and 3.68:1 on the page. Neither passes both. One color per theme is not a
// refinement here, it is the only way the mark is legible in both.
const (
	dark  = "#3fb950"
	light = "#1a7f37"
)

// Opacity by distance from the diagonal. The light ramp never goes as faint:
// 0.18 of a mid teal on white is indistinguishable from the page, which is how
// a five by five grid renders as a three by three one.
var (
	rampDark  = []string{"1", "0.55", "0.32", "0.18"}
	rampLight = []string{"1", "0.62", "0.42", "0.28"}
)

// The geometry every mark shares, and the name it carries. The four files
// differ in their color, their opacity ramp, how many cells fill the square
// and the gap between the cells; everything else about the drawing is the
// mark rather than the file. The canvas is SVG, so it is a coordinate system
// rather than a size in pixels.
const (
	markCanvas = 64
	markPad    = 8.0
	markRadius = 0.3
	brandName  = "ghchronicle"
)

func main() {
	// Nothing here cancels the run, but rsvg-convert is started under one
	// context so that a caller that wanted to could.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "mark":
		return markCmd(args[1:], stdout, stderr)
	case "compose":
		return composeCmd(ctx, args[1:], stdout, stderr)
	case "icons":
		return iconsCmd(ctx, args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "gen_brand: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// usage goes to stdout when it was asked for and to stderr when it accompanies
// an error, which is what argparse does: "mark.py -h | less" shows the help.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gen_brand <command> [-out dir]

  mark      the mark and the favicon, one file per theme
  compose   the banner, the social image and the og:image, SVG and PNG
  icons     what the documentation site serves to name itself: the favicon
            in SVG and ICO, the touch and manifest icons, the web manifest
            and the og:image (icons also takes -brand dir, where og.png is
            read from, brand by default)

-out defaults to the working directory, and for compose it is also where the
background rasters are read from.
`)
}

// out reads the one flag both subcommands share. It is hand-rolled rather than
// a flag.FlagSet so that an unknown flag reports the subcommand it was given
// to, which a shared set cannot do.
//
// When the arguments end the run instead, because they asked for the usage or
// cannot be read, proceed is false and status is what the run exits with; the
// usage or the complaint has already been written.
func out(args []string, stdout, stderr io.Writer) (dir string, status int, proceed bool) {
	dir = "."
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-out" || args[i] == "--out":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "gen_brand: -out needs a directory")
				return "", 2, false
			}
			dir = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-out="), strings.HasPrefix(args[i], "--out="):
			dir = args[i][strings.Index(args[i], "=")+1:]
		case args[i] == "-h" || args[i] == "-help" || args[i] == "--help":
			usage(stdout)
			return "", 0, false
		default:
			fmt.Fprintf(stderr, "gen_brand: unexpected argument %q\n", args[i])
			usage(stderr)
			return "", 2, false
		}
	}
	return dir, 0, true
}

// pyJoin is Python's os.path.join, which concatenates where filepath.Join
// cleans. compose prints the joined paths, and the documented invocation is
// "python3 compose.py" from inside brand/, so the default out directory is "."
// and Python reports "./social.svg". A cleaned join folds the "." away and
// reports "social.svg", and "-out ./sub" likewise loses its "./".
func pyJoin(dir, name string) string {
	sep := string(filepath.Separator)
	if dir == "" || strings.HasSuffix(dir, sep) || strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + sep + name
}

// fixed is Python's "%.2f", which rounds the exact binary value to nearest and
// breaks a tie to even. Go's formatter does the same, so every coordinate in
// the committed files reproduces byte for byte.
func fixed(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// repr is Python's repr of a float: the shortest decimal that reads back as the
// same double, with a trailing ".0" kept on a whole number. The compositions
// interpolate a bare float into the SVG, so "312.0" and "368.20000000000005"
// are both literally what is on disk today.
func repr(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// mark draws the grid. Row 0 is the top, so the diagonal rises left to right
// when the solid cell of column c sits at row (cells - 1 - c).
func mark(color string, ramp []string, cells int, gap float64) string {
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label=%q>`+"\n"+
			`  <g fill=%q>`+"\n"+"%s\n  </g>\n</svg>\n",
		markCanvas, markCanvas, markCanvas, markCanvas, brandName, color,
		strings.Join(markCells(cells, gap, func(d int) string {
			return fmt.Sprintf("opacity=%q", ramp[min(d, len(ramp)-1)])
		}), "\n"),
	)
}

// markCells is the grid's cells on the mark's own canvas, one rect per line,
// each closed by what paint says for its distance from the diagonal: an
// opacity for the mark, a class for the favicon, whose opacity changes with
// the color scheme.
func markCells(cells int, gap float64, paint func(distance int) string) []string {
	span := float64(markCanvas) - 2*markPad
	step := span / float64(cells)
	side := step - gap
	var out []string
	for r := range cells {
		for c := range cells {
			x := markPad + float64(c)*step
			y := markPad + float64(r)*step
			out = append(out, fmt.Sprintf(
				`    <rect x=%q y=%q width=%q height=%q rx=%q %s/>`,
				fixed(x), fixed(y), fixed(side), fixed(side), fixed(side*markRadius),
				paint(abs(cells-1-r-c)),
			))
		}
	}
	return out
}

func markCmd(args []string, stdout, stderr io.Writer) int {
	dir, status, proceed := out(args, stdout, stderr)
	if !proceed {
		return status
	}
	if err := os.MkdirAll(filepath.Clean(dir), 0o750); err != nil {
		return fail(stderr, err)
	}
	// The favicon is a different drawing on purpose: twenty five cells at
	// sixteen pixels is mush, so it drops to three by three and keeps the
	// diagonal, which is the part that carries the meaning.
	files := []struct {
		name string
		svg  string
	}{
		// The mark, one file per theme.
		{"mark-dark.svg", mark(dark, rampDark, 5, 2)},
		{"mark-light.svg", mark(light, rampLight, 5, 2)},
		{"favicon-dark.svg", mark(dark, faviconRampDark, 3, 4)},
		{"favicon-light.svg", mark(light, faviconRampLight, 3, 4)},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Clean(filepath.Join(dir, f.name)), []byte(f.svg), 0o600); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "wrote", f.name)
	}
	return 0
}

// The composition palette. The type is drawn over a raster whose density rises
// to the right, so it is light on dark and no theme pair is needed.
const (
	teal    = "#3fb950"
	heading = "#f2f6f8"
	body    = "#c3ccd2"
	font    = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"
)

const tagline = "Every metric GitHub will give, kept with the date it happened"

// The parts of a composition that are the same in all three: the grid the
// inlined mark is drawn on, and the gap between that mark and the type.
const (
	composeCells = 5
	composeGap   = 0.032
	textGap      = 36
)

type target struct {
	name       string
	background string
	w, h       int
	markSize   int
	titleSize  int
	tagSize    int
}

var targets = []target{
	{"social", "bg-social.png", 1280, 640, 200, 76, 30},
	{"og", "bg-og.png", 1200, 630, 190, 72, 29},
	{"banner", "bg-banner.png", 1280, 320, 132, 54, 22},
}

// markGroup inlines the mark rather than referencing it, so one SVG is one
// file. The geometry is the same grid, expressed in ratios of the drawn size
// because the three compositions each want a different one.
func markGroup(x, y, size int) string {
	pad := float64(size) * 0.125
	span := float64(size) - 2*pad
	step := span / float64(composeCells)
	gap := float64(size) * composeGap
	side := step - gap
	ramp := []string{"1", "0.55", "0.32", "0.18"}
	out := []string{fmt.Sprintf(`  <g fill=%q transform="translate(%d %d)">`, teal, x, y)}
	for r := range composeCells {
		for c := range composeCells {
			d := abs(composeCells - 1 - r - c)
			out = append(out, fmt.Sprintf(
				`    <rect x=%q y=%q width=%q height=%q rx=%q opacity=%q/>`,
				fixed(pad+float64(float64(c)*step)), fixed(pad+float64(float64(r)*step)),
				fixed(side), fixed(side), fixed(side*markRadius), ramp[min(d, len(ramp)-1)],
			))
		}
	}
	out = append(out, "  </g>")
	return strings.Join(out, "\n")
}

// baseline is the y of one line of type, a ratio of the mark's drawn size
// below the top of the block.
//
// The inner conversion is not decoration. The Go specification lets an
// implementation fuse a multiplication and the addition that consumes it into
// one operation rounded once, and arm64 does: fused, 220 + 190*0.78 is
// 368.2, and rounded separately, which is what the Python that wrote the
// committed files did, it is 368.20000000000005. The file on disk carries the
// second, so without the conversion this generator reproduces brand/ on an
// amd64 runner and fails to on an Apple one, which is how it was found.
// Converting the product forces the intermediate rounding on every
// architecture.
func baseline(top, size int, ratio float64) float64 {
	return float64(top) + float64(float64(size)*ratio)
}

// compose lays the block out left-aligned and vertically centered: the
// background's own density rises to the right, so the type sits where the field
// is quietest. The raster is embedded rather than linked because it is this SVG
// that gets rasterized, and a relative href would not survive being moved.
func compose(background string, t target) (string, error) {
	raw, err := os.ReadFile(background)
	if err != nil {
		return "", err
	}
	data := base64.StdEncoding.EncodeToString(raw)
	left := int(math.RoundToEven(float64(t.w) * 0.075))
	blockH := t.markSize
	top := int(math.RoundToEven(float64(t.h-blockH) / 2))
	textX := left + t.markSize + textGap
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s, %s">
  <image href="data:image/png;base64,%s" x="0" y="0" width="%d" height="%d" preserveAspectRatio="xMidYMid slice"/>
%s
  <text x="%d" y="%s" font-family="%s" font-size="%d" font-weight="700" fill="%s" dominant-baseline="middle">%s</text>
  <text x="%d" y="%s" font-family="%s" font-size="%d" font-weight="400" fill="%s" dominant-baseline="middle">%s</text>
</svg>
`,
		t.w, t.h, t.w, t.h, brandName, tagline,
		data, t.w, t.h,
		markGroup(left, top, t.markSize),
		textX, repr(baseline(top, t.markSize, 0.46)), font, t.titleSize, heading, brandName,
		textX, repr(baseline(top, t.markSize, 0.78)), font, t.tagSize, body, tagline,
	), nil
}

func composeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir, status, proceed := out(args, stdout, stderr)
	if !proceed {
		return status
	}
	// rsvg-convert is run by the absolute path exec.LookPath answers rather
	// than by a name exec would search PATH for (Sonar go:S4036). It is looked
	// up once, before the loop, so a machine without librsvg hears so before
	// the first SVG is replaced rather than after, with a stale PNG beside it.
	rsvg, lookErr := exec.LookPath("rsvg-convert")
	if lookErr != nil {
		return fail(stderr, lookErr)
	}
	for _, t := range targets {
		// The background is only ever opened, so it is cleaned here, once,
		// where it is built. The two paths below are also printed, and pyJoin
		// keeps those byte for byte what the Python this replaced reported.
		background := filepath.Clean(pyJoin(dir, t.background))
		svg, err := compose(background, t)
		if err != nil {
			return fail(stderr, err)
		}
		svgPath := pyJoin(dir, t.name+".svg")
		pngPath := pyJoin(dir, t.name+".png")
		if err = os.WriteFile(filepath.Clean(svgPath), []byte(svg), 0o600); err != nil {
			return fail(stderr, err)
		}
		// The PNG is what ships; the SVG is only the source it is cut from.
		// rsvg-convert runs inside the output directory and is given the two
		// bare names from the table above, so nothing off the command line
		// reaches its argument list.
		width, svgName, pngName := strconv.Itoa(t.w), t.name+".svg", t.name+".png"
		cmd := exec.CommandContext(ctx, rsvg, "-w", width, svgName, "-o", pngName)
		cmd.Dir = dir
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err = cmd.Run(); err != nil {
			return fail(stderr, fmt.Errorf("rsvg-convert %s: %w", svgPath, err))
		}
		fmt.Fprintln(stdout, "wrote", svgPath, "and", pngPath)
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// fail reports what stopped the run and answers with the status it exits with.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "gen_brand:", err)
	return 1
}
