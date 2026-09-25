package ghchronicle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.ps1 is install.sh's Windows half and owes its reader the same thing:
// it refuses whatever does not match what the release published. These run it
// on whatever platform the suite is on, because the parts worth testing, the
// lookup, the digest and the refusals, are not Windows-specific; the one part
// that is, editing the user PATH in the registry, is skipped by the script
// itself off Windows.
//
// They need pwsh. GitHub's Ubuntu runners have it, and a machine without it
// skips rather than pretending to have checked.

func powershell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("install.ps1 runs in PowerShell, and there is none here")
	return ""
}

// fakeWindowsRelease is a zip holding ghchronicle.exe and the checksum file
// that release would publish beside it, with the SBOM line that makes a
// substring lookup wrong.
func fakeWindowsRelease(t *testing.T, corrupt bool) (name string, zipped []byte, checksums string) {
	t.Helper()
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	w, err := zw.Create("ghchronicle.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("MZ not a real binary")); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipped = body.Bytes()
	name = fmt.Sprintf("ghchronicle_%s_windows_amd64.zip", fakeVersion)
	sum := sha256.Sum256(zipped)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		digest = strings.Repeat("0", len(digest))
	}
	checksums = fmt.Sprintf("%s  %s\n%s  %s.spdx.json\n",
		digest, name, strings.Repeat("1", 64), name)
	return name, zipped, checksums
}

// serveWindowsRelease serves the zip and its checksum file, and the signature
// bundle when signed. Unsigned for every test but the signature one, for the
// reason fakeRelease.signed gives.
func serveWindowsRelease(t *testing.T, name string, zipped []byte, checksums string, signed bool) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v"+fakeVersion+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipped)
	})
	mux.HandleFunc("/v"+fakeVersion+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	if signed {
		serveBundle(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runWindowsInstaller(t *testing.T, base, dir string, pathFirst ...string) (string, int) {
	t.Helper()
	shell := powershell(t)
	cmd := exec.CommandContext(t.Context(), shell, "-NoProfile", "-File", "install.ps1")
	cmd.Args = append(cmd.Args, "-Version", fakeVersion, "-BinDir", dir)
	env := append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+base,
		"GHCHRONICLE_LATEST_URL="+base+"/no-such-api",
		// The architecture Windows would report, so the run picks an archive
		// name whatever the machine underneath actually is.
		"PROCESSOR_ARCHITECTURE=AMD64")
	for _, first := range pathFirst {
		env = prependToPath(env, first)
	}
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// prependToPath puts dir in front of the PATH entry of environ, matching the
// name however the platform spells it: Windows says "Path", and appending a
// second "PATH=" would leave which of the two wins to the runtime rather than
// to the test.
func prependToPath(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+1)
	found := false
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, "PATH") {
			out = append(out, name+"="+dir+string(os.PathListSeparator)+value)
			found = true
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

// decoy writes something that answers to the binary's name and returns the
// directory holding it. It is not a working program and does not need to be:
// resolving a name against PATH reads the directory, it does not run what it
// finds. Both spellings, because Windows resolves a bare name through PATHEXT
// and finds the .exe, while PowerShell elsewhere finds the executable file
// named exactly as asked, which is also why this needs stubMode rather than a
// readable file.
func decoy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"ghchronicle", "ghchronicle.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), stubMode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTheWindowsInstallerPutsTheBinaryWhereItWasAsked(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, false)
	dir := t.TempDir()
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums, false), dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "checksum verified") {
		t.Errorf("the run says nothing about having checked what it downloaded:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghchronicle.exe")); err != nil {
		t.Fatalf("no binary was installed: %v\n%s", err, out)
	}
	// The other half of the shadow warning below: nothing else answers to the
	// name here, so a run that warns anyway would be crying wolf at every
	// install there is. Unless something does: a machine that runs
	// ghchronicle has one on PATH, and off Windows the installed .exe never
	// answers to the bare name, so there the warning is right and this half
	// has nothing to hold the run to.
	if other, err := exec.LookPath("ghchronicle"); err == nil {
		t.Logf("%s already answers to the name here, so the run was right to warn", other)
		return
	}
	if strings.Contains(out, "warning:") {
		t.Errorf("nothing else is on PATH, so this run had nothing to warn about:\n%s", out)
	}
}

// TestTheWindowsInstallerSaysWhenAnotherCopyKeepsWinningTheName covers the one
// outcome that reads as success and is not: the file is written, the PATH is
// updated, and the name still resolves somewhere else. Windows reads the
// machine PATH before the user one this script writes to, so a copy under
// Program Files, or an older `go install` build, keeps answering. install.sh
// has warned about this since it learned to; leaving it out here would have
// the two installers disagree about what a finished install means.
func TestTheWindowsInstallerSaysWhenAnotherCopyKeepsWinningTheName(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, false)
	dir := t.TempDir()
	earlier := decoy(t)
	out, code := runWindowsInstaller(t,
		serveWindowsRelease(t, name, zipped, checksums, false), dir, earlier)
	if code != 0 {
		t.Fatalf("exit %d, want 0: a shadowed install is still an install:\n%s", code, out)
	}
	if !strings.Contains(out, "still runs") {
		t.Errorf("the run does not say another copy keeps winning the name:\n%s", out)
	}
	if !strings.Contains(out, earlier) {
		t.Errorf("the warning does not name %s, which is the copy that wins:\n%s", earlier, out)
	}
}

func TestTheWindowsInstallerRefusesAnArchiveThatDoesNotMatchItsChecksum(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, true)
	dir := t.TempDir()
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums, false), dir)
	if code == 0 {
		t.Fatalf("a tampered archive was installed:\n%s", out)
	}
	if !strings.Contains(out, "does not match the checksum") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghchronicle.exe")); err == nil {
		t.Error("the refusal still left a binary behind, which is the one thing it must not do")
	}
}

// TestTheWindowsInstallerReadsTheChecksumLineForTheArchiveAndNotItsSBOM is the
// same trap install.sh met: the archive's name is a prefix of its SBOM's, and
// a lookup that matches both checks a file that was never downloaded.
func TestTheWindowsInstallerReadsTheChecksumLineForTheArchiveAndNotItsSBOM(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, false)
	if !strings.Contains(checksums, name+".spdx.json") {
		t.Fatal("this test needs a checksums file that also names the SBOM")
	}
	dir := t.TempDir()
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums, false), dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0. A checksums file naming the SBOM beside the archive is what "+
			"every real release publishes:\n%s", code, out)
	}
}

// TestTheWindowsInstallerChecksTheSignatureWhenCosignIsHere is install.sh's
// signature test run through install.ps1: the same cases, the same stand-in,
// and the same lines expected of it, because a reader on Windows is owed the
// same check and the same account of how far it went.
func TestTheWindowsInstallerChecksTheSignatureWhenCosignIsHere(t *testing.T) {
	t.Parallel()
	powershell(t)
	runSignatureCases(t, func(t *testing.T, first string, signed bool) (string, int, string) {
		t.Helper()
		name, zipped, checksums := fakeWindowsRelease(t, false)
		dir := t.TempDir()
		var ahead []string
		if first != "" {
			ahead = append(ahead, first)
		}
		out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums, signed), dir, ahead...)
		return out, code, filepath.Join(dir, "ghchronicle.exe")
	})
}
