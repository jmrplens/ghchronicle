package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// publicDir is the documentation site's public directory, which icons writes
// and which the site serves as it is.
const publicDir = "../../site/public"

// icoEntry is one directory entry of an ICO file, as ico writes it.
type icoEntry struct {
	W, H, Colors, Reserved byte
	Planes, Bits           uint16
	Bytes, Offset          uint32
}

// readICO parses an ICO file back into its entries and their payloads.
func readICO(t *testing.T, raw []byte) (entries []icoEntry, payloads [][]byte) {
	t.Helper()
	var header [3]uint16
	r := bytes.NewReader(raw)
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		t.Fatalf("the ICO header: %v", err)
	}
	if header[0] != 0 || header[1] != 1 {
		t.Fatalf("ICO header = %v, want reserved 0 and type 1", header)
	}
	entries = make([]icoEntry, header[2])
	if err := binary.Read(r, binary.LittleEndian, entries); err != nil {
		t.Fatalf("the ICO directory: %v", err)
	}
	for _, e := range entries {
		end := int(e.Offset) + int(e.Bytes)
		if end > len(raw) {
			t.Fatalf("an ICO entry reaches byte %d of %d", end, len(raw))
		}
		payloads = append(payloads, raw[e.Offset:end])
	}
	return entries, payloads
}

// TestIconsWritesWhatTheSiteServes runs icons with the stand-in rasterizer and
// no pngquant on PATH, and finds every file written, each raster asked for at
// its own size, and the text files identical to what the site serves.
func TestIconsWritesWhatTheSiteServes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "public")
	t.Setenv("PATH", rasterizer)
	status, stdout, stderr := genBrand(t, "icons", "-out", out, "-brand="+brandDir)
	if status != 0 || stderr != "" {
		t.Fatalf("icons = %d, %q, want a clean run", status, stderr)
	}
	names := []string{
		"favicon.svg", "manifest.webmanifest", "apple-touch-icon.png", "icon-192.png",
		"icon-512.png", "icon-maskable-512.png", "favicon.ico", "og.png",
	}
	want := "note: pngquant is not on PATH, so og.png is copied as compose wrote it\n" +
		"wrote " + strings.Join(names, "\nwrote ") + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	t.Run("the text files are what the site serves", func(t *testing.T) { sameAsPublic(t, out) })
	t.Run("each raster was asked for at its own size", func(t *testing.T) { askedAtEachSize(t, out) })
	t.Run("the ICO holds the three favicon rasters", func(t *testing.T) { icoHoldsTheRasters(t, out) })
	t.Run("og.png is copied untouched with no pngquant", func(t *testing.T) {
		if !bytes.Equal(readFile(t, filepath.Join(out, "og.png")), readFile(t, filepath.Join(brandDir, "og.png"))) {
			t.Error("og.png was changed although no pngquant was there to change it")
		}
	})
}

// sameAsPublic fails for each text file icons wrote into out that differs from
// the one the site serves.
func sameAsPublic(t *testing.T, out string) {
	t.Helper()
	for _, name := range []string{"favicon.svg", "manifest.webmanifest"} {
		if !bytes.Equal(readFile(t, filepath.Join(out, name)), readFile(t, filepath.Join(publicDir, name))) {
			t.Errorf("%s differs from site/public: run go run ./cmd/gen_brand icons -out site/public", name)
		}
	}
}

// askedAtEachSize reads what the stand-in wrote into each icon: the arguments
// it was run with.
func askedAtEachSize(t *testing.T, out string) {
	t.Helper()
	for _, ic := range appIcons {
		svgName := strings.TrimSuffix(ic.name, ".png") + ".svg"
		asked := fmt.Sprintf("-w %d %s -o %s", ic.size, svgName, ic.name)
		if got := readFile(t, filepath.Join(out, ic.name)); string(got) != asked {
			t.Errorf("rsvg-convert was asked %q, want %q", got, asked)
		}
	}
}

// icoHoldsTheRasters reads the ICO icons wrote back into its entries, and each
// entry's payload is what the stand-in made for that size.
func icoHoldsTheRasters(t *testing.T, out string) {
	t.Helper()
	entries, payloads := readICO(t, readFile(t, filepath.Join(out, "favicon.ico")))
	if len(entries) != len(icoSizes) {
		t.Fatalf("favicon.ico holds %d images, want %d", len(entries), len(icoSizes))
	}
	for i, size := range icoSizes {
		if int(entries[i].W) != size || int(entries[i].H) != size || entries[i].Bits != 32 {
			t.Errorf("entry %d = %+v, want %dx%d at 32 bits", i, entries[i], size, size)
		}
		asked := fmt.Sprintf("-w %d favicon-ico.svg -o favicon-%d.png", size, size)
		if string(payloads[i]) != asked {
			t.Errorf("entry %d holds %q, want the PNG rsvg-convert made for %q", i, payloads[i], asked)
		}
	}
}

// readFile reads a file the test needs and fails when it cannot.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestIconsWritesNothingUntilEveryRasterExists covers a rasterizer that is
// missing and one that refuses: in both, the site's directory is left as it
// was, rather than holding a new favicon beside stale PNGs.
func TestIconsWritesNothingUntilEveryRasterExists(t *testing.T) {
	t.Run("no rasterizer on PATH", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "public")
		t.Setenv("PATH", t.TempDir())
		status, stdout, stderr := genBrand(t, "icons", "-out", out, "-brand", brandDir)
		notFound := (&exec.Error{Name: "rsvg-convert", Err: exec.ErrNotFound}).Error()
		if status != 1 || stdout != "" || stderr != "gen_brand: "+notFound+"\n" {
			t.Errorf("icons = %d, stdout %q, stderr %q, want the lookup's own words", status, stdout, stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("the output directory: %v, want it never made", err)
		}
	})
	t.Run("a rasterizer that refuses", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "public")
		t.Setenv("PATH", rasterizer)
		t.Setenv("FAKERSVG_FAIL", "1")
		status, stdout, stderr := genBrand(t, "icons", "--out", out, "--brand", brandDir)
		if status != 1 || stdout != "" {
			t.Errorf("icons = %d, stdout %q, want a failure before anything is reported written", status, stdout)
		}
		if !strings.Contains(stderr, "gen_brand: rsvg-convert apple-touch-icon.svg: exit status 1") {
			t.Errorf("stderr = %q, want the rasterizer named with the file it refused", stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("the output directory: %v, want it never made", err)
		}
	})
}

// TestOGGoesThroughPngquantWhenItIsThere covers the three ways pngquant can
// end: its output served, the original kept with a note when it cannot hold
// the quality asked for, and a failure that names it with its own words.
func TestOGGoesThroughPngquantWhenItIsThere(t *testing.T) {
	brand := t.TempDir()
	if err := os.WriteFile(filepath.Join(brand, ogImage), []byte("og"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", quantizer)
	const kept = "note: pngquant could not keep og.png above the quality asked for, so it is copied as it is\n"
	for _, c := range []struct {
		name, exit, want, note, err string
	}{
		{"quantized", "", "quantized og", "", ""},
		{"below the quality asked for", "99", "og", kept, ""},
		{"refused", "1", "", "", "pngquant og.png: exit status 1: pngquant: refused as asked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FAKEPNGQUANT_EXIT", c.exit)
			var stdout strings.Builder
			og, err := optimizedOG(t.Context(), brand, t.TempDir(), &stdout)
			if (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) {
				t.Errorf("error = %v, want %q", err, c.err)
			}
			if string(og) != c.want || stdout.String() != c.note {
				t.Errorf("optimizedOG = %q, stdout %q, want %q and %q", og, stdout.String(), c.want, c.note)
			}
		})
	}
}

// TestOGRefusesAPngquantOnlyTheWorkingDirectoryHolds covers the one lookup
// failure that is not "absent": a pngquant exec.LookPath finds only relative
// to the working directory is somebody else's program, so it is refused
// rather than run or quietly skipped.
func TestOGRefusesAPngquantOnlyTheWorkingDirectoryHolds(t *testing.T) {
	brand := t.TempDir()
	if err := os.WriteFile(filepath.Join(brand, ogImage), []byte("og"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stand-in is linked in rather than written, because a link keeps
	// the mode that makes it a program exec.LookPath will find.
	name := programName("pngquant")
	here := t.TempDir()
	if err := os.Link(filepath.Join(quantizer, name), filepath.Join(here, name)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(here)
	t.Setenv("PATH", ".")
	var stdout strings.Builder
	og, err := optimizedOG(t.Context(), brand, t.TempDir(), &stdout)
	if !errors.Is(err, exec.ErrDot) || og != nil || stdout.Len() != 0 {
		t.Errorf("optimizedOG = %d bytes, %v, stdout %q, want exec.ErrDot and nothing said", len(og), err, stdout.String())
	}
}

// TestIconsReadsItsArguments covers the two flags in both spellings and the
// ways the arguments can end the run.
func TestIconsReadsItsArguments(t *testing.T) {
	cases := []struct {
		args         []string
		out, brand   string
		status       int
		proceed      bool
		stderrPrefix string
	}{
		{nil, ".", "brand", 0, true, ""},
		{[]string{"-out", "a", "-brand", "b"}, "a", "b", 0, true, ""},
		{[]string{"--out=a", "--brand=b"}, "a", "b", 0, true, ""},
		{[]string{"-out"}, "", "", 2, false, "gen_brand: -out needs a directory"},
		{[]string{"-brand"}, "", "", 2, false, "gen_brand: -brand needs a directory"},
		{[]string{"-size", "3"}, "", "", 2, false, `gen_brand: unexpected argument "-size"`},
		{[]string{"-h"}, "", "", 0, false, ""},
	}
	for _, tc := range cases {
		var stdout, stderr strings.Builder
		out, brand, status, proceed := iconsFlags(tc.args, &stdout, &stderr)
		if out != tc.out || brand != tc.brand || status != tc.status || proceed != tc.proceed ||
			!strings.HasPrefix(stderr.String(), tc.stderrPrefix) {
			t.Errorf("iconsFlags(%q) = %q, %q, %d, %v, stderr %q; want %q, %q, %d, %v, stderr starting %q",
				tc.args, out, brand, status, proceed, stderr.String(),
				tc.out, tc.brand, tc.status, tc.proceed, tc.stderrPrefix)
		}
	}
}

// TestEveryIconKeepsItsMarkCenteredAndTheMaskableOneInsideItsSafeZone holds the
// geometry, before anything is rasterized: the grid of every icon is centered,
// and the maskable icon's grid, corners included, falls inside the circle of
// 80% of the side a platform may cut it to.
func TestEveryIconKeepsItsMarkCenteredAndTheMaskableOneInsideItsSafeZone(t *testing.T) {
	t.Parallel()
	for _, ic := range appIcons {
		offset, side := gridBox(ic)
		if math.Abs(offset+side+offset-float64(ic.size)) > 1e-9 {
			t.Errorf("%s: the grid spans %v from %v, not centered on %d", ic.name, side, offset, ic.size)
		}
		if ic.purpose == "maskable" {
			if reach := maskableCornerReach(ic); reach >= 0.4 {
				t.Errorf("%s: the grid's corners reach %.3f of the side from the center, want under 0.4", ic.name, reach)
			}
		}
	}
}

// decodePNG reads a committed PNG and fails when it is not one.
func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return img
}

// TestTheSiteIconsAreWhatTheManifestAndTheHeadSay reads the files the site
// serves, which the stand-in cannot make, and holds each to its promise: the
// size the manifest gives, an opaque ground on every home screen icon, the
// maskable icon's ink inside its safe zone as rasterized, the three sizes the
// ICO's link lists, an og:image at the size its tags state, and a favicon that
// follows the color scheme.
func TestTheSiteIconsAreWhatTheManifestAndTheHeadSay(t *testing.T) {
	t.Parallel()
	t.Run("every manifest icon is there at its size", manifestIconsAtTheirSizes)
	t.Run("every home screen icon is opaque and the maskable one keeps to its safe zone", func(t *testing.T) {
		t.Parallel()
		for _, ic := range appIcons {
			checkAppIcon(t, ic, decodePNG(t, filepath.Join(publicDir, ic.name)))
		}
	})
	t.Run("the ICO holds three PNGs at the sizes its link lists", siteICOAtItsSizes)
	t.Run("og.png is the size its tags state", func(t *testing.T) {
		t.Parallel()
		if b := decodePNG(t, filepath.Join(publicDir, "og.png")).Bounds(); b.Dx() != 1200 || b.Dy() != 630 {
			t.Errorf("og.png is %dx%d, the og:image tags say 1200x630", b.Dx(), b.Dy())
		}
	})
	t.Run("the favicon follows the color scheme", faviconFollowsTheScheme)
}

// manifestIconsAtTheirSizes finds each icon the manifest lists, at the size it
// gives.
func manifestIconsAtTheirSizes(t *testing.T) {
	t.Parallel()
	var m webManifest
	if err := json.Unmarshal(readFile(t, filepath.Join(publicDir, "manifest.webmanifest")), &m); err != nil {
		t.Fatalf("manifest.webmanifest: %v", err)
	}
	for _, icon := range m.Icons {
		path := filepath.Join(publicDir, strings.TrimPrefix(icon.Src, siteBase))
		if icon.Type == "image/svg+xml" {
			readFile(t, path)
			continue
		}
		b := decodePNG(t, path).Bounds()
		if got := strconv.Itoa(b.Dx()) + "x" + strconv.Itoa(b.Dy()); got != icon.Sizes {
			t.Errorf("%s is %s, the manifest says %s", icon.Src, got, icon.Sizes)
		}
	}
}

// siteICOAtItsSizes reads the served ICO: three PNGs, at the sizes its link
// lists and its directory states.
func siteICOAtItsSizes(t *testing.T) {
	t.Parallel()
	entries, payloads := readICO(t, readFile(t, filepath.Join(publicDir, "favicon.ico")))
	if len(entries) != len(icoSizes) {
		t.Fatalf("favicon.ico holds %d images, want %d", len(entries), len(icoSizes))
	}
	for i, size := range icoSizes {
		img, err := png.Decode(bytes.NewReader(payloads[i]))
		if err != nil {
			t.Errorf("favicon.ico entry %d: %v", i, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != size || int(entries[i].W) != size {
			t.Errorf("favicon.ico entry %d is %dx%d under a directory saying %d, want %d", i, b.Dx(), b.Dy(), entries[i].W, size)
		}
	}
}

// faviconFollowsTheScheme reads the served SVG favicon: well formed, and
// carrying both greens and the rule that switches between them.
func faviconFollowsTheScheme(t *testing.T) {
	t.Parallel()
	svg := readFile(t, filepath.Join(publicDir, "favicon.svg"))
	if err := xml.Unmarshal(svg, new(struct{})); err != nil {
		t.Errorf("favicon.svg is not well formed: %v", err)
	}
	for _, want := range []string{"prefers-color-scheme:dark", light, dark} {
		if !bytes.Contains(svg, []byte(want)) {
			t.Errorf("favicon.svg lacks %q: it would not follow the browser's color scheme", want)
		}
	}
}

// checkAppIcon holds one rasterized home screen icon to its size and to an
// opaque ground, and a maskable one to its safe zone.
func checkAppIcon(t *testing.T, ic appIcon, img image.Image) {
	t.Helper()
	b := img.Bounds()
	if b.Dx() != ic.size || b.Dy() != ic.size {
		t.Errorf("%s is %dx%d, want %d", ic.name, b.Dx(), b.Dy(), ic.size)
	}
	if _, _, _, a := img.At(b.Min.X, b.Min.Y).RGBA(); a != 0xffff {
		t.Errorf("%s has a transparent corner, want the opaque ground a wallpaper cannot swallow", ic.name)
	}
	if ic.purpose != "maskable" {
		return
	}
	if far, limit := inkReach(img), 0.4*float64(ic.size); far > limit {
		t.Errorf("%s: ink reaches %.0f px from the center, past the %.0f px safe zone", ic.name, far, limit)
	}
}

// inkReach is how far from the center of a square icon its farthest inked
// pixel sits, inked meaning any pixel that is not the color of its corner.
func inkReach(img image.Image) float64 {
	b := img.Bounds()
	gr, gg, gb, _ := img.At(b.Min.X, b.Min.Y).RGBA()
	center := float64(b.Dx()) / 2
	far := 0.0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, g, bl, _ := img.At(x, y).RGBA(); r == gr && g == gg && bl == gb {
				continue
			}
			far = math.Max(far, math.Hypot(float64(x-b.Min.X)+0.5-center, float64(y-b.Min.Y)+0.5-center))
		}
	}
	return far
}

// TestICORefusesWhatTheFormatCannotHold covers the two sizes an ICO entry
// cannot spell.
func TestICORefusesWhatTheFormatCannotHold(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 257} {
		if _, err := ico([]icoImage{{size, []byte("png")}}); err == nil {
			t.Errorf("ico accepted an image %d pixels wide", size)
		}
	}
}

// TestICOPacksEachImageWhereItsEntrySays builds an ICO from real PNGs of three
// sizes, the 256 that the format writes as 0 among them, and reads it back.
func TestICOPacksEachImageWhereItsEntrySays(t *testing.T) {
	t.Parallel()
	var images []icoImage
	for _, size := range []int{16, 48, 256} {
		var buf bytes.Buffer
		if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, size, size))); err != nil {
			t.Fatal(err)
		}
		images = append(images, icoImage{size, buf.Bytes()})
	}
	packed, err := ico(images)
	if err != nil {
		t.Fatal(err)
	}
	entries, payloads := readICO(t, packed)
	for i, im := range images {
		// The format spells 256 as 0.
		want := im.size
		if want == 256 {
			want = 0
		}
		if int(entries[i].W) != want || int(entries[i].H) != want || entries[i].Planes != 1 {
			t.Errorf("entry %d = %+v, want width and height %d, one plane", i, entries[i], want)
		}
		if !bytes.Equal(payloads[i], im.png) {
			t.Errorf("entry %d does not point at its own PNG", i)
		}
	}
}
