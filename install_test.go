package ghchronicle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// install.sh is read off the network and piped into a shell, which is the
// least inspectable way anyone installs anything. The one thing it owes its
// reader is that it refuses whatever does not match what the release
// published, so these tests are mostly about refusing.
//
// They serve a release of their own rather than reaching for GitHub: the
// script takes its download base from the environment for exactly this, and a
// test that needs the network is a test that fails for reasons of its own.

const fakeVersion = "9.9.9"

// fakeRelease is a tar.gz holding one executable named ghchronicle that prints
// the line the real one prints, and the checksum file that release would
// publish beside it.
type fakeRelease struct {
	archiveName string
	archive     []byte
	checksums   string
}

func buildFakeRelease(t *testing.T, corrupt bool) fakeRelease {
	t.Helper()
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	tw := tar.NewWriter(zw)
	script := "#!/bin/sh\necho 'ghchronicle " + fakeVersion + " (fake)'\n"
	if err := tw.WriteHeader(&tar.Header{
		Name: "ghchronicle", Mode: 0o755, Size: int64(len(script)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	archive := body.Bytes()
	name := fmt.Sprintf("ghchronicle_%s_%s_%s.tar.gz", fakeVersion, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		// A digest that is the right shape and the wrong value, which is what a
		// tampered or truncated download looks like from here.
		digest = strings.Repeat("0", len(digest))
	}
	// The SBOM line matters: every archive's name is a prefix of its SBOM's, so
	// a lookup by substring picks up two lines and the check then runs against
	// a file the script never downloaded. That is a real failure this file was
	// written after meeting.
	checksums := fmt.Sprintf("%s  %s\n%s  %s.spdx.json\n",
		digest, name, strings.Repeat("1", 64), name)
	return fakeRelease{archiveName: name, archive: archive, checksums: checksums}
}

func serveRelease(t *testing.T, rel fakeRelease) string {
	t.Helper()
	return serveCounted(t, rel, nil)
}

// serveCounted is serveRelease with a counter, for the tests that care whether
// anything was fetched at all.
func serveCounted(t *testing.T, rel fakeRelease, asked *atomic.Int32) string {
	t.Helper()
	mux := http.NewServeMux()
	if asked != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			asked.Add(1)
			w.WriteHeader(http.StatusNotFound)
		})
	}
	mux.HandleFunc("/v"+fakeVersion+"/"+rel.archiveName, func(w http.ResponseWriter, _ *http.Request) {
		if asked != nil {
			asked.Add(1)
		}
		_, _ = w.Write(rel.archive)
	})
	mux.HandleFunc("/v"+fakeVersion+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		if asked != nil {
			asked.Add(1)
		}
		_, _ = w.Write([]byte(rel.checksums))
	})
	// Anything else is a 404, which is what asking for a version that was never
	// released looks like.
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runInstaller(t *testing.T, base, dir string, args ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("install.sh runs in bash, and there is none here")
	}
	// Built up rather than spread into the call: the arguments are this
	// file's own literals either way, and the spread form is what makes a
	// subprocess check read them as input from somewhere.
	cmd := exec.CommandContext(t.Context(), "bash", "install.sh")
	cmd.Args = append(cmd.Args, "--dir", dir)
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+base,
		// Nothing here may reach the real API. A test that resolves the newest
		// version over the network would start failing on release day.
		"GHCHRONICLE_LATEST_URL="+base+"/no-such-api",
		// PATH without cosign, so these exercise the path every plain machine
		// takes. The signature branch is covered by having run it by hand
		// against the published release, which is the only place a real
		// bundle exists.
		"PATH="+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestTheInstallerPutsTheBinaryWhereItWasAsked(t *testing.T) {
	t.Parallel()
	rel := buildFakeRelease(t, false)
	dir := t.TempDir()
	out, code := runInstaller(t, serveRelease(t, rel), dir, "--version", fakeVersion)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "checksum verified") {
		t.Errorf("the run says nothing about having checked what it downloaded:\n%s", out)
	}
	info, err := os.Stat(filepath.Join(dir, "ghchronicle"))
	if err != nil {
		t.Fatalf("no binary was installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the installed binary is not executable (%v)", info.Mode())
	}
}

func TestTheInstallerRefusesAnArchiveThatDoesNotMatchItsChecksum(t *testing.T) {
	t.Parallel()
	rel := buildFakeRelease(t, true)
	dir := t.TempDir()
	out, code := runInstaller(t, serveRelease(t, rel), dir, "--version", fakeVersion)
	if code == 0 {
		t.Fatalf("a tampered archive was installed:\n%s", out)
	}
	if !strings.Contains(out, "does not match the checksum") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghchronicle")); err == nil {
		t.Error("the refusal still left a binary behind, which is the one thing it must not do")
	}
}

func TestTheInstallerRefusesAVersionTheReleaseDoesNotHave(t *testing.T) {
	t.Parallel()
	rel := buildFakeRelease(t, false)
	dir := t.TempDir()
	out, code := runInstaller(t, serveRelease(t, rel), dir, "--version", "0.0.1")
	if code == 0 {
		t.Fatalf("a version that was never released installed something:\n%s", out)
	}
	if !strings.Contains(out, "no archive at") {
		t.Errorf("the refusal does not name what it could not find:\n%s", out)
	}
}

// TestTheInstallerReadsTheChecksumLineForTheArchiveAndNotItsSBOM pins the
// failure this file was written after meeting: every archive's name is a
// prefix of its SBOM's, and a substring lookup matches both.
func TestTheInstallerReadsTheChecksumLineForTheArchiveAndNotItsSBOM(t *testing.T) {
	t.Parallel()
	rel := buildFakeRelease(t, false)
	if !strings.Contains(rel.checksums, rel.archiveName+".spdx.json") {
		t.Fatal("this test needs a checksums file that also names the SBOM")
	}
	dir := t.TempDir()
	out, code := runInstaller(t, serveRelease(t, rel), dir, "--version", fakeVersion)
	if code != 0 {
		t.Fatalf("exit %d, want 0. A checksums file naming the SBOM beside the archive is what every "+
			"real release publishes:\n%s", code, out)
	}
}

// TestTheInstallerNeedsNoArgumentsToKnowWhatItCannotDo: piped into a shell
// there is nowhere to read a usage message from, so the refusals have to carry
// the whole answer.
func TestTheInstallerNeedsNoArgumentsToKnowWhatItCannotDo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("install.sh runs in bash, and there is none here")
	}
	cmd := exec.CommandContext(t.Context(), "bash", "install.sh", "--nonsense")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("an unknown option was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "--help") {
		t.Errorf("the refusal does not say where to look:\n%s", out)
	}
}

// TestTheInstallerStopsAtAPlatformWithNoRelease pins a bash rule that cost a
// wrong message: a command substitution inside a here-string is not the
// command `set -e` is watching, so `read os arch <<<"$(platform)"` printed the
// refusal and then carried on with both empty, spending a request to be told
// 404 for "ghchronicle_2.0.0__.tar.gz".
//
// uname is replaced with a shell function and the script is sourced, rather
// than a stub binary being put on PATH: a function needs no file and no
// executable bit, and the script runs exactly as it does otherwise.
func TestTheInstallerStopsAtAPlatformWithNoRelease(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("install.sh runs in bash, and there is none here")
	}
	var asked atomic.Int32
	base := serveCounted(t, buildFakeRelease(t, false), &asked)
	dir := t.TempDir()

	const shim = `uname() { case "$1" in -s) echo Linux ;; -m) echo mips64 ;; esac; }
source install.sh --dir "$1" --version "$2"`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", shim, "bash", dir, fakeVersion)
	cmd.Env = append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+base,
		"GHCHRONICLE_LATEST_URL="+base+"/no-such-api")
	out, _ := cmd.CombinedOutput()

	if cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("a platform with no release installed something:\n%s", out)
	}
	if !strings.Contains(string(out), "no release is built for mips64") {
		t.Errorf("the refusal does not name the platform:\n%s", out)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("it made %d request(s) after deciding it could not install anything:\n%s", n, out)
	}
}
