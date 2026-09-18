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

func serveWindowsRelease(t *testing.T, name string, zipped []byte, checksums string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v"+fakeVersion+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipped)
	})
	mux.HandleFunc("/v"+fakeVersion+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func runWindowsInstaller(t *testing.T, base, dir string) (string, int) {
	t.Helper()
	shell := powershell(t)
	cmd := exec.CommandContext(t.Context(), shell, "-NoProfile", "-File", "install.ps1")
	cmd.Args = append(cmd.Args, "-Version", fakeVersion, "-BinDir", dir)
	cmd.Env = append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+base,
		"GHCHRONICLE_LATEST_URL="+base+"/no-such-api",
		// The architecture Windows would report, so the run picks an archive
		// name whatever the machine underneath actually is.
		"PROCESSOR_ARCHITECTURE=AMD64")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestTheWindowsInstallerPutsTheBinaryWhereItWasAsked(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, false)
	dir := t.TempDir()
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums), dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "checksum verified") {
		t.Errorf("the run says nothing about having checked what it downloaded:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghchronicle.exe")); err != nil {
		t.Fatalf("no binary was installed: %v\n%s", err, out)
	}
}

func TestTheWindowsInstallerRefusesAnArchiveThatDoesNotMatchItsChecksum(t *testing.T) {
	t.Parallel()
	name, zipped, checksums := fakeWindowsRelease(t, true)
	dir := t.TempDir()
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums), dir)
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
	out, code := runWindowsInstaller(t, serveWindowsRelease(t, name, zipped, checksums), dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0. A checksums file naming the SBOM beside the archive is what "+
			"every real release publishes:\n%s", code, out)
	}
}
