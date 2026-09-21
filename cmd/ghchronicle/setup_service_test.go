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
		"/etc/ghchronicle/ghchronicle.env")
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
