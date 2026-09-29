package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestACommandLineNamesTheConfigurationSoTheShellReadsItBack: a path any
// shell reads as one word is written as it is, and any other is quoted in
// the quotes the system's shells take, single on POSIX and double on
// Windows, where cmd keeps a single quote as part of the word.
func TestACommandLineNamesTheConfigurationSoTheShellReadsItBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goos, path, want string }{
		{"linux", "/etc/ghchronicle/config.yaml", "/etc/ghchronicle/config.yaml"},
		{"linux", "config.yaml", "config.yaml"},
		{"linux", "/srv/my config.yaml", "'/srv/my config.yaml'"},
		{"linux", "/srv/it's.yaml", `'/srv/it'\''s.yaml'`},
		{"linux", "/srv/$HOME/config.yaml", "'/srv/$HOME/config.yaml'"},
		{"linux", `/srv/back\slash.yaml`, `'/srv/back\slash.yaml'`},
		{"linux", "~/config.yaml", "'~/config.yaml'"},
		{"darwin", "/Users/José/config.yaml", "/Users/José/config.yaml"},
		{"linux", "", "''"},
		{"windows", `C:\ProgramData\ghchronicle\config.yaml`, `C:\ProgramData\ghchronicle\config.yaml`},
		{"windows", "C:/ProgramData/ghchronicle/config.yaml", "C:/ProgramData/ghchronicle/config.yaml"},
		{
			"windows", `C:\Users\John Smith\AppData\Roaming\ghchronicle\config.yaml`,
			`"C:\Users\John Smith\AppData\Roaming\ghchronicle\config.yaml"`,
		},
		{
			"windows", `C:\Users\RUNNER~1\AppData\Local\Temp\config.yaml`,
			`"C:\Users\RUNNER~1\AppData\Local\Temp\config.yaml"`,
		},
		{"windows", `C:\srv\it's.yaml`, `"C:\srv\it's.yaml"`},
		{"windows", `C:\a&b\config.yaml`, `"C:\a&b\config.yaml"`},
		{"windows", `C:\a,b\config.yaml`, `"C:\a,b\config.yaml"`},
		{"windows", "", `""`},
	} {
		if got := QuotePathFor(tc.goos, tc.path); got != tc.want {
			t.Errorf("QuotePathFor(%s, %q) = %s, want %s", tc.goos, tc.path, got, tc.want)
		}
	}
}

// TestACommandLineIsTheBinaryTheQuotedPathAndTheFlags, on the system the
// test runs on, with a path of that system.
func TestACommandLineIsTheBinaryTheQuotedPathAndTheFlags(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "my config.yaml")
	got := CommandLine(path, "-migrate", "-yes")
	want := "ghchronicle -config " + QuotePath(path) + " -migrate -yes"
	if got != want || !strings.Contains(got, path) || QuotePath(path) == path {
		t.Errorf("CommandLine(%q) = %s, want %s, with the path quoted for its space", path, got, want)
	}
}
