package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// What the guided setup writes to make this run by itself, on each system.
//
// Only one of the three can be run on the machine a test runs on, and the
// other two are the ones a mistake would sit in longest: nobody notices a
// broken plist until somebody with a Mac tries it. So the files are built and
// read here on every system, and the matrix runs this on all three.

// TestTheSystemdUnitSaysWhatSystemdNeeds, and takes away what a collector does
// not need: it reads a token, writes one state file and talks to GitHub.
func TestTheSystemdUnitSaysWhatSystemdNeeds(t *testing.T) {
	t.Parallel()
	unit := systemdUnit("/usr/local/bin/ghchronicle", "/etc/ghchronicle/config.yaml",
		"/etc/ghchronicle/ghchronicle.env", host{goos: "linux", root: true})
	for _, want := range []string{
		"ExecStart=/usr/local/bin/ghchronicle -config /etc/ghchronicle/config.yaml",
		"EnvironmentFile=/etc/ghchronicle/ghchronicle.env",
		"Restart=on-failure",
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"[Install]",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not carry %q:\n%s", want, unit)
		}
	}
	// Whatever it may write has to be allowed through ProtectSystem=strict, or
	// the first sweep fails writing its state file.
	if !strings.Contains(unit, "ReadWritePaths=") {
		t.Error("the unit protects the filesystem and lets nothing through for the state file")
	}
}

// TestTheLaunchdAgentIsAPlistThatLaunchdTakes. It has no EnvironmentFile, and
// what replaces it is worth being explicit about rather than silently absent.
func TestTheLaunchdAgentIsAPlistThatLaunchdTakes(t *testing.T) {
	t.Parallel()
	plist := launchdPlist("/usr/local/bin/ghchronicle", "/Users/x/.config/ghchronicle/config.yaml",
		"/Users/x/.config/ghchronicle/ghchronicle.env")
	for _, want := range []string{
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`,
		"<key>Label</key><string>io.jmrp.ghchronicle</string>",
		"<string>/usr/local/bin/ghchronicle</string>",
		"<string>-config</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("the plist does not carry %q:\n%s", want, plist)
		}
	}
	if strings.Count(plist, "<dict>") != strings.Count(plist, "</dict>") ||
		strings.Count(plist, "<array>") != strings.Count(plist, "</array>") {
		t.Errorf("the plist does not close what it opens:\n%s", plist)
	}
}

// TestTheWindowsTaskIsXMLSchtasksTakes. The encoding line is not decoration:
// schtasks /create /xml refuses a file that does not declare UTF-16.
func TestTheWindowsTaskIsXMLSchtasksTakes(t *testing.T) {
	t.Parallel()
	task := windowsTask(`C:\Users\x\AppData\Local\Programs\ghchronicle\ghchronicle.exe`,
		`C:\Users\x\AppData\Roaming\ghchronicle\config.yaml`)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"`,
		"<BootTrigger><Enabled>true</Enabled></BootTrigger>",
		`<Command>C:\Users\x\AppData\Local\Programs\ghchronicle\ghchronicle.exe</Command>`,
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the task does not carry %q:\n%s", want, task)
		}
	}
	if strings.Count(task, "<Task") != 1 || !strings.Contains(task, "</Task>") {
		t.Errorf("the task is not one closed Task element:\n%s", task)
	}
}

// TestEverySystemHasSomewhereToPutItAndSomethingToWrite, so a reader on any of
// the three is offered the question rather than silently skipped.
func TestEverySystemHasSomewhereToPutItAndSomethingToWrite(t *testing.T) {
	t.Parallel()
	kind := serviceKind()
	path := servicePath()
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if kind == "" {
			t.Errorf("serviceKind() is empty on %s, so the question is never asked", runtime.GOOS)
		}
		if path == "" {
			t.Errorf("servicePath() is empty on %s, so nothing is ever written", runtime.GOOS)
		}
		if !filepath.IsAbs(path) {
			t.Errorf("servicePath() = %q, want somewhere absolute", path)
		}
	default:
		if kind != "" {
			t.Errorf("serviceKind() = %q on %s, which nothing here knows how to write",
				kind, runtime.GOOS)
		}
	}
}

// TestTheCommandsToStartItMatchTheFileItWrote. The file is useless without
// them, and a command naming another init system is worse than none.
func TestTheCommandsToStartItMatchTheFileItWrote(t *testing.T) {
	t.Parallel()
	body, start := serviceFile("/bin/ghchronicle", "/c/config.yaml", "/c/env", servicePath())
	if body == "" || len(start) == 0 {
		t.Fatalf("nothing written or nothing to run on %s", runtime.GOOS)
	}
	want := map[string]string{
		"linux":   "systemctl",
		"darwin":  "launchctl",
		"windows": "schtasks",
	}[runtime.GOOS]
	if want != "" && !strings.Contains(strings.Join(start, " "), want) {
		t.Errorf("the commands are %v, want them to use %s on %s", start, want, runtime.GOOS)
	}
}

// TestTheConfigurationLandsSomewhereTheAccountCanWrite. A setup that finishes
// by failing to write is the one outcome it exists to avoid.
func TestTheConfigurationLandsSomewhereTheAccountCanWrite(t *testing.T) {
	t.Parallel()
	path := defaultConfigPath()
	if !filepath.IsAbs(path) && path != "config.yaml" {
		t.Errorf("defaultConfigPath() = %q, want an absolute path or the local fallback", path)
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 &&
		strings.HasPrefix(path, "/etc/") {
		t.Errorf("defaultConfigPath() = %q for a run that is not root", path)
	}
}

// The three systems, from a machine that is only one of them.
//
// What these read is not the text of a unit, which the tests above already do,
// but whether the code that decides where it goes and what starts it runs at
// all for a system this is not. Reading runtime.GOOS inside those decisions
// meant two of the three were only ever compiled.

// TestWhereItGoesOnEachSystem, including the difference between a machine's
// own service and an account's, which is the one a reader is most likely to
// meet and the one nobody running as root ever sees.
func TestWhereItGoesOnEachSystem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		on   host
		want string
		kind string
	}{
		{
			"linux as root",
			host{goos: "linux", root: true, home: "/root"},
			"/etc/systemd/system/ghchronicle.service", "a systemd service",
		},
		{
			"linux as an account",
			host{goos: "linux", home: "/home/x"},
			"/home/x/.config/systemd/user/ghchronicle.service", "a systemd service",
		},
		{
			"macOS",
			host{goos: "darwin", home: "/Users/x"},
			"/Users/x/Library/LaunchAgents/io.jmrp.ghchronicle.plist", "a launchd agent",
		},
		{
			"windows",
			host{goos: "windows", appdata: `C:\Users\x\AppData\Roaming`},
			`C:\Users\x\AppData\Roaming/ghchronicle/ghchronicle-task.xml`, "a scheduled task",
		},
		{"windows with no APPDATA", host{goos: "windows"}, "", "a scheduled task"},
		{"something else entirely", host{goos: "plan9", home: "/usr/x"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.on.servicePath(); got != filepath.FromSlash(tc.want) &&
				got != tc.want {
				t.Errorf("servicePath() = %q, want %q", got, tc.want)
			}
			if got := tc.on.kind(); got != tc.kind {
				t.Errorf("kind() = %q, want %q", got, tc.kind)
			}
		})
	}
}

// TestWhatStartsItOnEachSystem. A command naming another system's init is
// worse than no command: it is one a reader will try.
func TestWhatStartsItOnEachSystem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		on    host
		says  string
		first string
	}{
		{"linux as root", host{goos: "linux", root: true}, "systemctl enable --now", "systemctl daemon-reload"},
		{"linux as an account", host{goos: "linux"}, "systemctl --user enable --now", "systemctl --user daemon-reload"},
		{"macOS", host{goos: "darwin", home: "/Users/x"}, "launchctl load", "launchctl load"},
		{"windows", host{goos: "windows", appdata: `C:\x`}, "schtasks /create", "schtasks /create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, start := tc.on.serviceFile("/bin/ghchronicle", "/c/config.yaml", "/c/env",
				tc.on.servicePath())
			if body == "" {
				t.Fatal("nothing to write")
			}
			joined := strings.Join(start, "\n")
			if !strings.Contains(joined, tc.says) {
				t.Errorf("commands = %v, want them to carry %q", start, tc.says)
			}
			if len(start) == 0 || !strings.HasPrefix(start[0], strings.Fields(tc.first)[0]) {
				t.Errorf("the first command is %q, want it to start with %q", start, tc.first)
			}
		})
	}
	// And a system this does not know writes nothing rather than a unit for
	// the wrong one.
	body, start := host{goos: "plan9"}.serviceFile("/bin/x", "/c", "/e", "")
	if body != "" || len(start) != 0 {
		t.Errorf("plan9 got %q and %v, want nothing at all", body, start)
	}
}

// TestTheUnitInstallsWhereTheAccountCanStartIt: a machine's own service is
// wanted by multi-user, an account's by its own default target, and getting
// that backwards is a unit that enables and never runs.
func TestTheUnitInstallsWhereTheAccountCanStartIt(t *testing.T) {
	t.Parallel()
	asRoot := systemdUnit("/bin/x", "/c", "/e", host{goos: "linux", root: true})
	asUser := systemdUnit("/bin/x", "/c", "/e", host{goos: "linux"})
	if !strings.Contains(asRoot, "WantedBy=multi-user.target") {
		t.Errorf("the machine's unit:\n%s", asRoot)
	}
	if !strings.Contains(asUser, "WantedBy=default.target") {
		t.Errorf("the account's unit:\n%s", asUser)
	}
}

// TestWhereTheConfigurationAndTheStateGoOnEachSystem.
func TestWhereTheConfigurationAndTheStateGoOnEachSystem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		on            host
		userConfigDir string
		config, state string
	}{
		{
			"root on a unix",
			host{goos: "linux", root: true},
			"/root/.config",
			"/etc/ghchronicle/config.yaml", "/var/lib/ghchronicle/state.json",
		},
		{
			"an account on a unix",
			host{goos: "linux"},
			"/home/x/.config",
			"/home/x/.config/ghchronicle/config.yaml", "/home/x/.config/ghchronicle/state.json",
		},
		{
			"macOS",
			host{goos: "darwin"},
			"/Users/x/Library/Application Support",
			"/Users/x/Library/Application Support/ghchronicle/config.yaml",
			"/Users/x/Library/Application Support/ghchronicle/state.json",
		},
		{
			"windows",
			host{goos: "windows", appdata: `C:\Users\x\AppData\Roaming`},
			`C:\Users\x\AppData\Roaming`,
			`C:\Users\x\AppData\Roaming/ghchronicle/config.yaml`,
			`C:\Users\x\AppData\Roaming/ghchronicle/state.json`,
		},
		{"nowhere to put it", host{goos: "linux"}, "", "config.yaml", "state.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.on.configPath(tc.userConfigDir); got != filepath.FromSlash(tc.config) &&
				got != tc.config {
				t.Errorf("configPath() = %q, want %q", got, tc.config)
			}
			if got := tc.on.statePath(tc.userConfigDir); got != filepath.FromSlash(tc.state) &&
				got != tc.state {
				t.Errorf("statePath() = %q, want %q", got, tc.state)
			}
		})
	}
}

// TestATypedSecretIsOnlyHiddenWhereSomethingCanHideIt. A pipe cannot be told
// to stop echoing, so there the answer is to read it plainly rather than to
// pretend: a nil reader is what the caller checks to decide that.
func TestATypedSecretIsOnlyHiddenWhereSomethingCanHideIt(t *testing.T) {
	t.Parallel()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if hiddenReader(f) != nil {
		t.Error("it offered to hide what is typed into something that is not a terminal")
	}
}

// TestWithNowhereToPutItTheSetupStillHasAnAnswer. A machine with neither
// XDG_CONFIG_HOME nor HOME set, which is what a bare container gives, cannot
// be asked where an account's configuration belongs. The setup writes into the
// working directory rather than stopping, because the point of the guided run
// is to end with a file that exists.
func TestWithNowhereToPutItTheSetupStillHasAnAnswer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	config, state := defaultConfigPath(), defaultStatePath()
	// As root the machine's own place is the answer whatever the environment
	// says, and it is an absolute one; for anybody else it is the directory
	// this was run from.
	if os.Geteuid() == 0 {
		if config != "/etc/ghchronicle/config.yaml" || state != "/var/lib/ghchronicle/state.json" {
			t.Errorf("as root: %q and %q", config, state)
		}
		return
	}
	if config != configName || state != "state.json" {
		t.Errorf("with nothing to go on: %q and %q, want them beside the working directory",
			config, state)
	}
}
