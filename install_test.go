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
	return buildFakeReleaseWith(t, runtime.GOOS, runtime.GOARCH, corrupt)
}

// buildFakeReleaseFor is buildFakeRelease for a platform this machine is not,
// which is how the macOS path is covered without a Mac.
func buildFakeReleaseFor(t *testing.T, goos, goarch string) fakeRelease {
	t.Helper()
	return buildFakeReleaseWith(t, goos, goarch, false)
}

func buildFakeReleaseWith(t *testing.T, goos, goarch string, corrupt bool) fakeRelease {
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
	name := fmt.Sprintf("ghchronicle_%s_%s_%s.tar.gz", fakeVersion, goos, goarch)
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

// serveNamed reads as what it is at the call site of a platform test: a server
// holding one named archive.
func serveNamed(t *testing.T, rel fakeRelease) string {
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

// requireBash skips a test that has nothing to say on this machine.
//
// Windows is skipped although its runners do have bash, through Git for
// Windows: install.sh refuses to run there on purpose and points at the zip,
// so driving it would be asserting that a refusal refuses. What Windows is
// for is install.ps1, which has tests of its own.
func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh refuses on Windows by design; install.ps1 is what runs there")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("install.sh runs in bash, and there is none here")
	}
}

func runInstaller(t *testing.T, base, dir string, args ...string) (string, int) {
	t.Helper()
	requireBash(t)
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
	requireBash(t)
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
	requireBash(t)
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

// sourceTargetDir runs the script's target_dir with the environment a test
// sets up, by sourcing it with the final call to main stripped out. Calling the
// function is the point: the alternative is asserting on a whole install, which
// says nothing about which directory was chosen and why.
func sourceTargetDir(t *testing.T, env ...string) string {
	t.Helper()
	requireBash(t)
	const probe = `source <(grep -v '^main "$@"$' install.sh); target_dir`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", probe)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sourcing install.sh: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestTheInstallerPrefersADirectoryTheShellAlreadySearches.
//
// On a Debian-like system ~/.local/bin is put on PATH by ~/.profile only when
// it already exists, so an install that creates it leaves the command not found
// until the next login. Somewhere already on PATH is worth preferring for that
// reason alone.
func TestTheInstallerPrefersADirectoryTheShellAlreadySearches(t *testing.T) {
	t.Parallel()
	if unix, err := os.OpenFile("/usr/local/bin/.ghchronicle-write-probe",
		os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = unix.Close()
		_ = os.Remove("/usr/local/bin/.ghchronicle-write-probe")
		t.Skip("/usr/local/bin is writable here, so a system-wide install is right and this choice never arises")
	}
	home := t.TempDir()
	for _, sub := range []string{".local/bin", "bin"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Only ~/bin is on PATH, and it is the second candidate, so picking it is
	// the preference and not the order of the list.
	got := sourceTargetDir(t, "HOME="+home,
		"PATH="+filepath.Join(home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if want := filepath.Join(home, "bin"); got != want {
		t.Errorf("it would install into %s, want %s: that one is already on PATH and the other is not", got, want)
	}
}

// TestTheInstallerSaysHowToFinishWhenItLandsOffPath. "Add it to your PATH" on
// its own leaves the reader to work out both the line and the file it goes in,
// and does not mention that there is a system-wide install at all.
func TestTheInstallerSaysHowToFinishWhenItLandsOffPath(t *testing.T) {
	t.Parallel()
	rel := buildFakeRelease(t, false)
	dir := filepath.Join(t.TempDir(), "nowhere-near-path")
	out, code := runInstaller(t, serveRelease(t, rel), dir, "--version", fakeVersion)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{
		`export PATH="` + dir + `:$PATH"`,
		"| sudo bash",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the closing advice does not carry %q:\n%s", want, out)
		}
	}
}

// TestTheInstallerTakesTheDarwinArchiveOnAMac. There is no macOS in this
// suite, so uname is replaced and the naming, download, digest and install run
// exactly as they do anywhere else. What this cannot cover is the tooling
// difference: a Mac has shasum and no sha256sum, which the script handles by
// looking for both.
func TestTheInstallerTakesTheDarwinArchiveOnAMac(t *testing.T) {
	t.Parallel()
	requireBash(t)
	rel := buildFakeReleaseFor(t, "darwin", "arm64")
	dir := t.TempDir()

	const shim = `uname() { case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; esac; }
source install.sh --dir "$1" --version "$2"`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", shim, "bash", dir, fakeVersion)
	cmd.Env = append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+serveNamed(t, rel),
		"GHCHRONICLE_LATEST_URL=http://127.0.0.1:1/no-such-api")
	out, _ := cmd.CombinedOutput()

	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit %d, want 0:\n%s", cmd.ProcessState.ExitCode(), out)
	}
	if !strings.Contains(string(out), "for darwin/arm64") {
		t.Errorf("it did not go looking for the macOS archive:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghchronicle")); err != nil {
		t.Fatalf("no binary was installed: %v\n%s", err, out)
	}
}

// TestTheInstallerVerifiesWithShasumWhenThereIsNoSha256sum is the macOS
// branch. A Mac has shasum, from Perl's Digest::SHA, and no sha256sum, so the
// one line that checks the download is a different program there. Forced by
// running with a PATH that holds everything the script needs except that one.
func TestTheInstallerVerifiesWithShasumWhenThereIsNoSha256sum(t *testing.T) {
	t.Parallel()
	requireBash(t)
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("this machine has no shasum, which is the program being stood in for")
	}
	// Everything the run touches, by name, so what is left out is left out on
	// purpose rather than by accident.
	shims := t.TempDir()
	for _, tool := range []string{
		"bash", "curl", "tar", "gzip", "awk", "grep", "sed",
		"shasum", "install", "mktemp", "uname", "tr", "head", "mkdir", "rm",
	} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("this machine has no %s, so the trimmed PATH cannot be built", tool)
		}
		if err = os.Symlink(path, filepath.Join(shims, tool)); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", "install.sh")
	cmd.Args = append(cmd.Args, "--dir", dir, "--version", fakeVersion)
	cmd.Env = append(os.Environ(),
		"GHCHRONICLE_DOWNLOAD_BASE="+serveRelease(t, buildFakeRelease(t, false)),
		"GHCHRONICLE_LATEST_URL=http://127.0.0.1:1/no-such-api",
		"PATH="+shims)
	out, _ := cmd.CombinedOutput()

	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit %d, want 0. Without sha256sum the check has to fall to shasum:\n%s",
			cmd.ProcessState.ExitCode(), out)
	}
	if !strings.Contains(string(out), "checksum verified") {
		t.Errorf("it installed without saying it had checked anything:\n%s", out)
	}
}

// TestTheInstallerSaysWhenAnolderCopyStillWins.
//
// On PATH is not the same as the one that runs. An older copy from `go
// install` in ~/go/bin, or a package manager's, earlier in PATH keeps winning,
// and nothing about the install would say so: the version it prints at the end
// comes from the file just written, by its full path, so the run looks right
// while the name resolves elsewhere. That happened on a real machine, where a
// build from three weeks earlier shadowed a release for an afternoon.
func TestTheInstallerSaysWhenAnOlderCopyStillWins(t *testing.T) {
	t.Parallel()
	requireBash(t)
	shadow := t.TempDir()
	impostor := filepath.Join(shadow, "ghchronicle")
	// A link to something already executable, rather than a script this test
	// would have to chmod: what is being stood up is a name earlier in PATH,
	// and command -v cares that it resolves, not what it does.
	target, err := exec.LookPath("true")
	if err != nil {
		t.Skip("this machine has no true(1) to stand in for an older copy")
	}
	if err = os.Symlink(target, impostor); err != nil {
		t.Fatal(err)
	}

	for name, first := range map[string]bool{
		"an older copy comes first": true,
		"nothing else is on PATH":   false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// The target is on PATH either way, so what changes between the
			// two runs is only whether something else beats it to the name.
			path := dir
			if first {
				path = shadow + string(os.PathListSeparator) + dir
			}
			cmd := exec.CommandContext(t.Context(), "bash", "install.sh")
			cmd.Args = append(cmd.Args, "--dir", dir, "--version", fakeVersion)
			cmd.Env = append(os.Environ(),
				"GHCHRONICLE_DOWNLOAD_BASE="+serveRelease(t, buildFakeRelease(t, false)),
				"GHCHRONICLE_LATEST_URL=http://127.0.0.1:1/no-such-api",
				"PATH="+path+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, _ := cmd.CombinedOutput()

			if cmd.ProcessState.ExitCode() != 0 {
				t.Fatalf("exit %d, want 0:\n%s", cmd.ProcessState.ExitCode(), out)
			}
			warned := strings.Contains(string(out), "still runs "+impostor)
			switch {
			case first && !warned:
				t.Errorf("it installed over a shadowed name and said nothing, so the reader believes "+
					"the version it printed is the one their shell will run:\n%s", out)
			case !first && strings.Contains(string(out), "still runs"):
				t.Errorf("it warned about a copy that is not there:\n%s", out)
			}
		})
	}
}

// TestTheInstallerOffersTheGuidedSetup, and asks through /dev/tty rather than
// standard input.
//
// The documented way to run this is `curl ... | bash`, where standard input is
// the script itself: a `read` there swallows the rest of the script instead of
// waiting for a person, which is the shape of bug that turns an install into a
// half-run script with no error.
func TestTheInstallerOffersTheGuidedSetup(t *testing.T) {
	t.Parallel()
	body := readInstaller(t)
	if !strings.Contains(body, "-setup") {
		t.Error("the installer never mentions the guided setup")
	}
	offer := section(t, body, "offer_setup()")
	for _, want := range []string{"/dev/tty", "read -r answer < /dev/tty"} {
		if !strings.Contains(offer, want) {
			t.Errorf("offer_setup does not use %q, so `curl | bash` would read the script:\n%s",
				want, offer)
		}
	}
	// And the check is an open rather than a permission test: `[ -r /dev/tty ]`
	// says yes on a machine with no controlling terminal and the open then
	// fails, which broke every non-interactive install until this test caught
	// it.
	if !strings.Contains(offer, ": < /dev/tty") {
		t.Errorf("offer_setup does not try to open the terminal before asking for one:\n%s", offer)
	}
	if strings.Contains(shellOnly(offer), "-r /dev/tty") {
		t.Error("offer_setup tests permissions on /dev/tty, which is not the question")
	}
}

// readInstaller is install.sh as text, for the tests that read it rather than
// run it.
func readInstaller(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// shellOnly is the lines a shell would run, without the comments. A test about
// what the script does must not be answered by a comment explaining what it
// deliberately does not do.
func shellOnly(body string) string {
	var kept []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// section is the body of a shell function, for a test that wants to read one
// rather than the whole file.
func section(t *testing.T, body, opening string) string {
	t.Helper()
	start := strings.Index(body, opening)
	if start < 0 {
		t.Fatalf("no %s in the installer", opening)
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s is not closed", opening)
	}
	return body[start : start+end]
}
