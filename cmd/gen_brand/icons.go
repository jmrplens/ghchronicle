package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The documentation site's base path and the page color of its dark theme,
// which is the ground every opaque icon sits on: a transparent or white icon
// disappears against the light and dark wallpapers a home screen actually has.
const (
	siteBase = "/ghchronicle/"
	pageDark = "#0e1316"
)

// The favicon's two opacity ramps, one per theme, shared by the per-theme
// files mark writes and the one file icons writes that carries both.
var (
	faviconRampDark  = []string{"1", "0.42", "0.2"}
	faviconRampLight = []string{"1", "0.5", "0.3"}
)

// maskableSpan is the share of the side the grid spans on the maskable icon.
// A platform may cut a maskable icon to any shape that keeps a centered circle
// of 80% of the side, so the grid's corners, which carry the diagonal's two
// solid ends, have to fall inside that circle: a square whose half diagonal is
// 0.4 of the side spans 0.8/sqrt(2), about 0.566, and this leaves a margin
// under it for the antialiased edge.
const maskableSpan = 0.54

// appIcon is one raster icon: its file, its size, which drawing of the mark it
// carries, how much of the side the grid spans, and its purpose in the web
// manifest, empty for the one icon the manifest does not list.
type appIcon struct {
	name    string
	size    int
	cells   int
	gap     float64
	ramp    []string
	span    float64
	purpose string
}

// The iOS touch icon keeps the favicon's three by three drawing: it is shown
// at sixty points, where twenty five cells begin to blur, and iOS rounds its
// corners itself, so the grid keeps clear of them. The manifest icons are the
// full mark.
var appIcons = []appIcon{
	{"apple-touch-icon.png", 180, 3, 4, faviconRampDark, 0.66, ""},
	{"icon-192.png", 192, 5, 2, rampDark, 0.72, "any"},
	{"icon-512.png", 512, 5, 2, rampDark, 0.72, "any"},
	{"icon-maskable-512.png", 512, 5, 2, rampDark, maskableSpan, "maskable"},
}

// icoSizes are the sizes the legacy favicon.ico carries, for the clients that
// still ask for /favicon.ico by name.
var icoSizes = []int{16, 32, 48}

// faviconSVG is the three by three favicon as one file for both themes: the
// light theme's green and ramp by default, and the dark theme's under a
// prefers-color-scheme rule, so a tab on a dark browser chrome gets the green
// that reads there. The per-theme files mark writes stay as they are; this is
// the one the site serves.
func faviconSVG() string {
	var css strings.Builder
	fmt.Fprintf(&css, "g{fill:%s}", light)
	for d := 1; d < len(faviconRampLight); d++ {
		fmt.Fprintf(&css, ".d%d{opacity:%s}", d, faviconRampLight[d])
	}
	fmt.Fprintf(&css, "@media (prefers-color-scheme:dark){g{fill:%s}", dark)
	for d := 1; d < len(faviconRampDark); d++ {
		fmt.Fprintf(&css, ".d%d{opacity:%s}", d, faviconRampDark[d])
	}
	css.WriteString("}")
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label=%q>`+"\n"+
			"  <style>%s</style>\n  <g>\n%s\n  </g>\n</svg>\n",
		markCanvas, markCanvas, markCanvas, markCanvas, brandName, css.String(),
		strings.Join(markCells(3, 4, func(d int) string {
			return fmt.Sprintf("class=%q", "d"+strconv.Itoa(min(d, len(faviconRampLight)-1)))
		}), "\n"),
	)
}

// gridBox is where an icon's grid sits: its offset from the top left corner,
// the same on both axes because it is centered, and its side, in pixels.
func gridBox(ic appIcon) (offset, side float64) {
	side = ic.span * float64(ic.size)
	return (float64(ic.size) - side) / 2, side
}

// iconSVG draws an icon: the dark page as an opaque ground, and the mark
// scaled from its own canvas onto the centered box gridBox gives.
func iconSVG(ic appIcon) string {
	offset, side := gridBox(ic)
	span := float64(markCanvas) - 2*markPad
	scale := side / span
	shift := offset - markPad*scale
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">`+"\n"+
			`  <rect width="%d" height="%d" fill=%q/>`+"\n"+
			`  <g fill=%q transform="translate(%s %s) scale(%s)">`+"\n%s\n  </g>\n</svg>\n",
		ic.size, ic.size, ic.size, ic.size, ic.size, ic.size, pageDark,
		dark, num(shift), num(shift), num(scale),
		strings.Join(markCells(ic.cells, ic.gap, func(d int) string {
			return fmt.Sprintf("opacity=%q", ic.ramp[min(d, len(ic.ramp)-1)])
		}), "\n"),
	)
}

// icoImage is one size of favicon.ico, as the PNG it is stored as.
type icoImage struct {
	size int
	png  []byte
}

// ico packs PNGs into an ICO file. Every browser since Internet Explorer 9 and
// every Windows since Vista reads a PNG stored in an ICO entry as it is, so
// nothing has to be converted to the older bitmap layout. A size of 256 is
// written as 0, which is how the format spells it, and one the format cannot
// hold is refused rather than wrapped.
func ico(images []icoImage) ([]byte, error) {
	const header, entry = 6, 16
	count, err := u16(len(images))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, count})
	offset := header + entry*len(images)
	for _, im := range images {
		var side byte
		switch {
		case im.size == 256:
		case im.size > 0 && im.size < 256:
			side = byte(im.size)
		default:
			return nil, fmt.Errorf("an ICO image is 1 to 256 pixels wide, not %d", im.size)
		}
		size, sizeErr := u32(len(im.png))
		at, atErr := u32(offset)
		if joined := errors.Join(sizeErr, atErr); joined != nil {
			return nil, joined
		}
		_ = binary.Write(&buf, binary.LittleEndian, struct {
			W, H, Colors, Reserved byte
			Planes, Bits           uint16
			Bytes, Offset          uint32
		}{side, side, 0, 0, 1, 32, size, at})
		offset += len(im.png)
	}
	for _, im := range images {
		buf.Write(im.png)
	}
	return buf.Bytes(), nil
}

// u16 and u32 are a count or a length as the ICO field that holds it, refused
// when it does not fit rather than wrapped.
func u16(n int) (uint16, error) {
	if n < 0 || n > math.MaxUint16 {
		return 0, fmt.Errorf("%d does not fit the 16 bits an ICO counts its images in", n)
	}
	return uint16(n), nil
}

func u32(n int) (uint32, error) {
	if n < 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("%d does not fit the 32 bits an ICO measures its images in", n)
	}
	return uint32(n), nil
}

// manifestIcon and webManifest are the web manifest in the order its fields
// are written, so a regeneration that changes nothing rewrites nothing.
type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

type webManifest struct {
	Name            string         `json:"name"`
	ShortName       string         `json:"short_name"`
	Description     string         `json:"description"`
	StartURL        string         `json:"start_url"`
	Scope           string         `json:"scope"`
	Display         string         `json:"display"`
	BackgroundColor string         `json:"background_color"`
	ThemeColor      string         `json:"theme_color"`
	Icons           []manifestIcon `json:"icons"`
}

// manifest is the web manifest that lists the icons. The theme color is the
// light theme's green, the brand color the sibling sites put in theirs, and
// the background the dark page, which is what a launch shows before the page
// has painted.
func manifest() string {
	m := webManifest{
		Name:            brandName + " documentation",
		ShortName:       brandName,
		Description:     tagline,
		StartURL:        siteBase,
		Scope:           siteBase,
		Display:         "standalone",
		BackgroundColor: pageDark,
		ThemeColor:      light,
		Icons: []manifestIcon{
			{siteBase + "favicon.svg", "any", "image/svg+xml", "any"},
		},
	}
	for _, ic := range appIcons {
		if ic.purpose == "" {
			continue
		}
		size := strconv.Itoa(ic.size)
		m.Icons = append(m.Icons, manifestIcon{siteBase + ic.name, size + "x" + size, "image/png", ic.purpose})
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		// Only strings and a slice of them: there is nothing here Marshal can
		// refuse.
		panic(err)
	}
	return string(out) + "\n"
}

// iconsFlags reads the icons subcommand's two directories: where the site's
// public files go, and the brand directory og.png is taken from.
func iconsFlags(args []string, stdout, stderr io.Writer) (outDir, brand string, status int, proceed bool) {
	outDir, brand = ".", "brand"
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[i], "-"), "=")
		name = strings.TrimPrefix(name, "-")
		switch name {
		case "out", "brand":
			if !hasValue {
				if i+1 >= len(args) {
					fmt.Fprintf(stderr, "gen_brand: -%s needs a directory\n", name)
					return "", "", 2, false
				}
				value = args[i+1]
				i++
			}
			if name == "out" {
				outDir = value
			} else {
				brand = value
			}
		case "h", "help":
			usage(stdout)
			return "", "", 0, false
		default:
			fmt.Fprintf(stderr, "gen_brand: unexpected argument %q\n", args[i])
			usage(stderr)
			return "", "", 2, false
		}
	}
	return outDir, brand, 0, true
}

// rasterJob is one SVG icons hands rsvg-convert: its text, the width to render
// it at, and the two bare names it goes by in the scratch directory.
type rasterJob struct {
	svg              string
	width            int
	svgName, pngName string
}

// rasterJobs is every raster icons makes, from the tables above: the touch
// and manifest icons, then the three sizes of the ICO, which keeps the dark
// theme's green on a transparent ground, what it has always been, because it
// is read by clients that know nothing of the color scheme and that green
// still reads on a light tab.
func rasterJobs() []rasterJob {
	var jobs []rasterJob
	for _, ic := range appIcons {
		jobs = append(jobs, rasterJob{iconSVG(ic), ic.size, strings.TrimSuffix(ic.name, ".png") + ".svg", ic.name})
	}
	favicon := mark(dark, faviconRampDark, 3, 4)
	for _, size := range icoSizes {
		jobs = append(jobs, rasterJob{favicon, size, "favicon-ico.svg", "favicon-" + strconv.Itoa(size) + ".png"})
	}
	return jobs
}

// optimizedOG is the og:image the site serves: brand/og.png through pngquant
// when pngquant is on PATH, which takes it from half a megabyte to under two
// hundred kilobytes with a mean difference under one level in 255, and the
// file as it is otherwise, with a note saying so.
func optimizedOG(ctx context.Context, brand, work string, stdout io.Writer) ([]byte, error) {
	source, err := os.ReadFile(filepath.Clean(filepath.Join(brand, "og.png")))
	if err != nil {
		return nil, err
	}
	quant, found := onPath("pngquant")
	if !found {
		fmt.Fprintln(stdout, "note: pngquant is not on PATH, so og.png is copied as compose wrote it")
		return source, nil
	}
	// Copied into the scratch directory and quantized there by two fixed
	// names, the way rsvg-convert is run, so nothing off the command line
	// reaches pngquant's argument list. The directory is opened as a root so
	// neither name can reach outside it.
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if writeErr := root.WriteFile("og-source.png", source, 0o600); writeErr != nil {
		return nil, writeErr
	}
	cmd := exec.CommandContext(ctx, quant, "--quality=80-95", "--speed=1", "--strip", "--force",
		"--output", "og.png", "--", "og-source.png")
	cmd.Dir = work
	out, runErr := cmd.CombinedOutput()
	switch {
	case runErr == nil:
		return root.ReadFile("og.png")
	case belowQuality(runErr):
		fmt.Fprintln(stdout, "note: pngquant could not keep og.png above the quality asked for, so it is copied as it is")
		return source, nil
	default:
		return nil, fmt.Errorf("pngquant og.png: %w: %s", runErr, bytes.TrimSpace(out))
	}
}

// onPath is the absolute path exec.LookPath finds for a program, and whether
// it found one: a program that is not there is an answer here, not an error.
func onPath(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

// belowQuality is pngquant's exit status 99, "the result would fall below the
// quality asked for", which is a reason to keep the original rather than a
// failure.
func belowQuality(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 99
}

// namedFile is one file icons writes into the site, held until all of them
// exist.
type namedFile struct {
	name string
	data []byte
}

// iconsCmd writes everything the documentation site serves to name itself: the
// favicon in SVG and ICO, the touch and manifest icons, the web manifest and
// the og:image. It renders every raster into a scratch directory first and
// writes into the site only once all of them exist, so a rasterizer that fails
// half way leaves the site's files as they were.
func iconsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	outDir, brand, status, proceed := iconsFlags(args, stdout, stderr)
	if !proceed {
		return status
	}
	// Looked up by absolute path, as compose does (Sonar go:S4036).
	rsvg, err := exec.LookPath("rsvg-convert")
	if err != nil {
		return fail(stderr, err)
	}
	work, err := os.MkdirTemp("", "gen-brand-icons")
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	files := []namedFile{
		{"favicon.svg", []byte(faviconSVG())},
		{"manifest.webmanifest", []byte(manifest())},
	}
	rendered := map[string][]byte{}
	for _, job := range rasterJobs() {
		if err = os.WriteFile(filepath.Join(work, job.svgName), []byte(job.svg), 0o600); err != nil {
			return fail(stderr, err)
		}
		// By bare names inside the scratch directory, as compose runs it, so
		// nothing off the command line reaches its argument list.
		cmd := exec.CommandContext(ctx, rsvg, "-w", strconv.Itoa(job.width), job.svgName, "-o", job.pngName)
		cmd.Dir = work
		cmd.Stderr = stderr
		if err = cmd.Run(); err != nil {
			return fail(stderr, fmt.Errorf("rsvg-convert %s: %w", job.svgName, err))
		}
		png, readErr := os.ReadFile(filepath.Join(work, job.pngName))
		if readErr != nil {
			return fail(stderr, readErr)
		}
		rendered[job.pngName] = png
	}
	for _, ic := range appIcons {
		files = append(files, namedFile{ic.name, rendered[ic.name]})
	}
	var images []icoImage
	for _, size := range icoSizes {
		images = append(images, icoImage{size, rendered["favicon-"+strconv.Itoa(size)+".png"]})
	}
	packed, err := ico(images)
	if err != nil {
		return fail(stderr, err)
	}
	files = append(files, namedFile{"favicon.ico", packed})
	og, err := optimizedOG(ctx, brand, work, stdout)
	if err != nil {
		return fail(stderr, err)
	}
	files = append(files, namedFile{"og.png", og})

	if err = os.MkdirAll(filepath.Clean(outDir), 0o750); err != nil {
		return fail(stderr, err)
	}
	for _, f := range files {
		if err = os.WriteFile(filepath.Clean(filepath.Join(outDir, f.name)), f.data, 0o600); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "wrote", f.name)
	}
	return 0
}

// maskableCornerReach is how far from the center the grid's farthest corner
// sits on an icon, as a share of its side: under 0.4 means the whole mark
// survives any mask a platform cuts a maskable icon to.
func maskableCornerReach(ic appIcon) float64 {
	offset, _ := gridBox(ic)
	center := float64(ic.size) / 2
	return math.Hypot(center-offset, center-offset) / float64(ic.size)
}
