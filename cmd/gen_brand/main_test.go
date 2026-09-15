package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// brandDir is the committed brand directory, which both families must
// reproduce byte for byte.
const brandDir = "../../brand"

// rasterizer is the directory holding the stand-in for rsvg-convert TestMain
// builds out of testdata/rsvg-convert.
var rasterizer string

// rasterizerName is the name the stand-in has to have for PATH to find it as
// the rsvg-convert gen_brand runs by bare name: that name everywhere but
// Windows, where exec.LookPath only finds a program through an extension
// PATHEXT lists.
func rasterizerName() string {
	if runtime.GOOS == "windows" {
		return "rsvg-convert.exe"
	}
	return "rsvg-convert"
}

// TestMain builds the stand-in for rsvg-convert once, for the tests of the
// compose family. A real one would make the PNGs, which is slow, needs
// librsvg, and proves nothing about this command that the SVG does not.
func TestMain(m *testing.M) {
	os.Exit(runWithRasterizer(m))
}

// runWithRasterizer builds the stand-in, runs the tests and cleans up after
// them.
func runWithRasterizer(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gen-brand-rasterizer")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create a build directory for the stand-in: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Bounded, because the test timeout cannot stop a build started before
	// m.Run.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	path := filepath.Join(dir, rasterizerName())
	build := exec.CommandContext(ctx, "go", "build", "-o", path, "./testdata/rsvg-convert")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "build testdata/rsvg-convert: %v\n%s", buildErr, out)
		return 1
	}
	rasterizer = dir
	return m.Run()
}

// genBrand runs the command and returns its status and both streams.
func genBrand(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(t.Context(), args, &out, &errOut)
	return status, out.String(), errOut.String()
}

// sameAsCommitted fails for every named file in dir that differs from the
// committed one of the same name.
func sameAsCommitted(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(brandDir, name))
		if err != nil {
			t.Fatalf("the committed %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the committed file: the generator and brand/ have drifted apart", name)
		}
	}
}

// TestMarkReproducesTheCommittedFiles regenerates the mark and the favicons
// and finds them identical to what brand/ ships.
func TestMarkReproducesTheCommittedFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "made", "here")
	status, stdout, stderr := genBrand(t, "mark", "-out", dir)
	if status != 0 || stderr != "" {
		t.Fatalf("mark = %d, %q, want a clean run", status, stderr)
	}
	names := []string{"mark-dark.svg", "mark-light.svg", "favicon-dark.svg", "favicon-light.svg"}
	sameAsCommitted(t, dir, names...)
	want := "wrote " + strings.Join(names, "\nwrote ") + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestMarkTakesTheOutDirectoryInEverySpelling writes into the directory given
// whichever of the four spellings of the flag names it. A spelling the reader
// did not know would not fall back to the working directory quietly: it is an
// unexpected argument, so the run would fail and write nothing.
func TestMarkTakesTheOutDirectoryInEverySpelling(t *testing.T) {
	t.Parallel()
	for _, spelling := range []func(dir string) []string{
		func(dir string) []string { return []string{"-out", dir} },
		func(dir string) []string { return []string{"--out", dir} },
		func(dir string) []string { return []string{"-out=" + dir} },
		func(dir string) []string { return []string{"--out=" + dir} },
	} {
		dir := t.TempDir()
		args := append([]string{"mark"}, spelling(dir)...)
		if status, _, stderr := genBrand(t, args...); status != 0 || stderr != "" {
			t.Errorf("gen_brand %s = %d, %q, want a clean run", strings.Join(args, " "), status, stderr)
			continue
		}
		sameAsCommitted(t, dir, "mark-dark.svg", "mark-light.svg", "favicon-dark.svg", "favicon-light.svg")
	}
}

// withBackgrounds is a fresh output directory holding the committed background
// rasters, with the stand-in first and alone on PATH.
func withBackgrounds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, tgt := range targets {
		raw, readErr := os.ReadFile(filepath.Join(brandDir, tgt.background))
		if readErr != nil {
			t.Fatalf("the committed background %s: %v", tgt.background, readErr)
		}
		if err = out.WriteFile(tgt.background, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", rasterizer)
	return dir
}

// TestComposeReproducesTheCommittedFiles rebuilds the three compositions from
// the committed backgrounds and hands each to the rasterizer inside the output
// directory, by bare name and at its own width.
func TestComposeReproducesTheCommittedFiles(t *testing.T) {
	dir := withBackgrounds(t)
	status, stdout, stderr := genBrand(t, "compose", "-out="+dir)
	if status != 0 || stderr != "" {
		t.Fatalf("compose = %d, %q, want a clean run", status, stderr)
	}
	sameAsCommitted(t, dir, "social.svg", "og.svg", "banner.svg")
	for _, tgt := range targets {
		png, err := os.ReadFile(filepath.Join(dir, tgt.name+".png"))
		if err != nil {
			t.Errorf("%s.png was not rasterized: %v", tgt.name, err)
			continue
		}
		want := fmt.Sprintf("-w %d %s.svg -o %s.png", tgt.w, tgt.name, tgt.name)
		if string(png) != want {
			t.Errorf("rsvg-convert was asked %q, want %q", png, want)
		}
		// The platform's own separator, which is the one compose joins with,
		// the way Python's os.path.join does.
		sep := string(filepath.Separator)
		line := "wrote " + dir + sep + tgt.name + ".svg and " + dir + sep + tgt.name + ".png\n"
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout = %q, want %q", stdout, line)
		}
	}
}

// TestComposeStopsAtTheFirstFailure covers the three things compose depends
// on: the background, a place to write the SVG, and the rasterizer.
func TestComposeStopsAtTheFirstFailure(t *testing.T) {
	t.Run("a background that is not there", func(t *testing.T) {
		dir := withBackgrounds(t)
		if err := os.Remove(filepath.Join(dir, targets[0].background)); err != nil {
			t.Fatal(err)
		}
		status, _, stderr := genBrand(t, "compose", "--out", dir)
		if status != 1 || !strings.HasPrefix(stderr, "gen_brand: open ") {
			t.Errorf("compose = %d, %q, want the missing background named", status, stderr)
		}
	})
	t.Run("an SVG that cannot be written", func(t *testing.T) {
		dir := withBackgrounds(t)
		if err := os.Mkdir(filepath.Join(dir, targets[0].name+".svg"), 0o750); err != nil {
			t.Fatal(err)
		}
		status, _, stderr := genBrand(t, "compose", "-out", dir)
		if status != 1 || !strings.Contains(stderr, "is a directory") {
			t.Errorf("compose = %d, %q, want the write refused", status, stderr)
		}
	})
	t.Run("a rasterizer that refuses", func(t *testing.T) {
		dir := withBackgrounds(t)
		t.Setenv("FAKERSVG_FAIL", "1")
		status, stdout, stderr := genBrand(t, "compose", "-out", dir)
		if status != 1 || stdout != "" {
			t.Errorf("compose = %d, stdout %q, want a failure before anything is reported written", status, stdout)
		}
		want := "rsvg-convert: Error reading SVG: XML parse error\n" +
			"gen_brand: rsvg-convert " + dir + string(filepath.Separator) + "social.svg: exit status 1\n"
		if stderr != want {
			t.Errorf("stderr = %q, want the rasterizer's own complaint, then which file it was", stderr)
		}
	})
}

// TestMarkStopsWhereItCannotWrite covers an output path that is a file, and a
// mark whose name is taken by a directory.
func TestMarkStopsWhereItCannotWrite(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, stderr := genBrand(t, "mark", "-out", file); status != 1 || !strings.HasPrefix(stderr, "gen_brand: mkdir ") {
		t.Errorf("mark into a file = %d, %q, want the directory refused", status, stderr)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "mark-dark.svg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if status, stdout, stderr := genBrand(t, "mark", "-out", dir); status != 1 || stdout != "" ||
		!strings.Contains(stderr, "is a directory") {
		t.Errorf("mark over a directory = %d, %q, %q, want the first write refused", status, stdout, stderr)
	}
}

// TestRunReadsItsArguments pins the statuses argparse would give: the usage
// asked for is a success on stdout, anything unreadable is a 2 on stderr.
func TestRunReadsItsArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   []string
		status int
		stdout string
		stderr string
	}{
		{"no command", nil, 2, "", "usage: gen_brand"},
		{"help", []string{"help"}, 0, "usage: gen_brand", ""},
		{"-h", []string{"-h"}, 0, "usage: gen_brand", ""},
		{"-help", []string{"-help"}, 0, "usage: gen_brand", ""},
		{"--help", []string{"--help"}, 0, "usage: gen_brand", ""},
		{"an unknown command", []string{"paint"}, 2, "", "gen_brand: unknown command \"paint\"\nusage: gen_brand"},
		{"help for a command", []string{"mark", "-h"}, 0, "usage: gen_brand", ""},
		{"-help for a command", []string{"mark", "-help"}, 0, "usage: gen_brand", ""},
		{"--help for a command", []string{"compose", "--help"}, 0, "usage: gen_brand", ""},
		{"-out with no directory", []string{"compose", "-out"}, 2, "", "gen_brand: -out needs a directory\n"},
		{"an unexpected argument", []string{"mark", "brand"}, 2, "", "gen_brand: unexpected argument \"brand\"\nusage: gen_brand"},
	} {
		status, stdout, stderr := genBrand(t, tc.args...)
		if status != tc.status || !strings.HasPrefix(stdout, tc.stdout) || !strings.HasPrefix(stderr, tc.stderr) ||
			(tc.stdout == "") != (stdout == "") || (tc.stderr == "") != (stderr == "") {
			t.Errorf("%s: run = %d, %q, %q, want %d, %q..., %q...", tc.name, status, stdout, stderr,
				tc.status, tc.stdout, tc.stderr)
		}
	}
}

// TestPyJoinConcatenatesLikePython keeps the "./" a cleaned join would fold
// away, and adds no separator where the directory already ends in one.
func TestPyJoinConcatenatesLikePython(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	for _, tc := range []struct{ dir, want string }{
		{".", "." + sep + "social.svg"},
		{"./sub", "./sub" + sep + "social.svg"},
		{"brand/", "brand/social.svg"},
		{"", "social.svg"},
	} {
		if got := pyJoin(tc.dir, "social.svg"); got != tc.want {
			t.Errorf("pyJoin(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// TestReprPrintsAFloatLikePython keeps the ".0" on a whole number and the
// full shortest form on anything else.
func TestReprPrintsAFloatLikePython(t *testing.T) {
	t.Parallel()
	for v, want := range map[float64]string{312: "312.0", 368.20000000000005: "368.20000000000005", -2.5: "-2.5"} {
		if got := repr(v); got != want {
			t.Errorf("repr(%v) = %q, want %q", v, got, want)
		}
	}
}

// TestTheBaselineIsRoundedTheWayTheCommittedFilesWere pins the one value the
// Go specification allows an architecture to compute differently.
//
// `float64(top) + float64(size)*ratio` may be fused into a single rounding,
// and arm64 fuses it: the og composition's tagline then reads 368.2 where the
// committed file, written by the Python this replaced, says
// 368.20000000000005. TestComposeReproducesTheCommittedFiles catches it only
// on a machine that fuses, which is why this exists beside it: it states the
// number, so the conversion cannot be removed as noise on a machine where
// removing it changes nothing.
func TestTheBaselineIsRoundedTheWayTheCommittedFilesWere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		top, size int
		ratio     float64
		want      string
	}{
		{220, 200, 0.46, "312.0"},
		{220, 200, 0.78, "376.0"},
		{220, 190, 0.46, "307.4"},
		{220, 190, 0.78, "368.20000000000005"},
		{94, 132, 0.46, "154.72"},
		{94, 132, 0.78, "196.96"},
	} {
		if got := repr(baseline(c.top, c.size, c.ratio)); got != c.want {
			t.Errorf("baseline(%d, %d, %g) = %s, want %s: the product is being rounded once with the sum", c.top, c.size, c.ratio, got, c.want)
		}
	}
}
