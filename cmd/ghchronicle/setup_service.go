package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Running it by itself, on each system.
//
// Three systems, three ways, and the same rule for all of them: write the file
// and then say the one or two commands that start it, rather than running them
// and hoping. Somebody who reads what it wrote before enabling it is being
// careful, not slow, and the commands are the same either way.

// serviceMode is what a unit, an agent or a task is written with. It names a
// binary and a path and nothing secret, and the thing that reads it is the
// init system rather than the account that wrote it.
const serviceMode = 0o644

// host is the three things about a machine that decide where a service goes
// and what it is called.
//
// Passed rather than read, so that the launchd agent and the Windows task are
// exercised on whatever machine the tests run on. Reading runtime.GOOS inside
// each of these meant two of the three paths were only ever compiled, and the
// one thing worth knowing about a file for a system you are not on is whether
// the code that writes it runs at all.
type host struct {
	goos    string
	root    bool
	home    string
	appdata string
}

// thisHost is the machine this is running on.
func thisHost() host {
	home, _ := os.UserHomeDir()
	return host{
		goos:    runtime.GOOS,
		root:    runtime.GOOS != "windows" && os.Geteuid() == 0,
		home:    home,
		appdata: os.Getenv("APPDATA"),
	}
}

// serviceKind is what this system calls a thing that runs by itself, or "" for
// one this does not know how to write.
func serviceKind() string { return thisHost().kind() }

// kind is the same for a named machine.
func (h host) kind() string {
	switch h.goos {
	case "linux":
		return "a systemd service"
	case "darwin":
		return "a launchd agent"
	case "windows":
		return "a scheduled task"
	default:
		return ""
	}
}

// servicePath is where the file goes, per system and per whether this is root.
func servicePath() string { return thisHost().servicePath() }

// servicePath is the same for a named machine.
func (h host) servicePath() string {
	switch h.goos {
	case "linux":
		if h.root {
			return "/etc/systemd/system/ghchronicle.service"
		}
		return filepath.Join(h.home, ".config", "systemd", "user", "ghchronicle.service")
	case "darwin":
		return filepath.Join(h.home, "Library", "LaunchAgents", "io.jmrp.ghchronicle.plist")
	case "windows":
		if h.appdata != "" {
			return filepath.Join(h.appdata, "ghchronicle", "ghchronicle-task.xml")
		}
	}
	return ""
}

// writeService writes it and says how to start it.
func writeService(ask *asker, answers setupAnswers, configPath string) error {
	path := servicePath()
	if path == "" {
		return nil
	}
	return writeServiceAt(ask, answers, configPath, path)
}

// writeServiceAt is the same with the destination given, which is what lets a
// test watch it refuse to replace one.
func writeServiceAt(ask *asker, answers setupAnswers, configPath, path string) error {
	// Before anything is written. A machine that already runs this has a unit
	// at this path, and replacing it without asking is how a guided setup run
	// to try something out takes down the thing it was trying it against.
	if _, err := os.Stat(path); err == nil {
		replace, askErr := ask.yes(path+" is already there. Replace it?", false)
		if askErr != nil {
			return askErr
		}
		if !replace {
			ask.sayf("Left it alone. The configuration above is written; point the")
			ask.sayf("existing service at it, or run this again with -config elsewhere.")
			return nil
		}
	}
	binary, err := os.Executable()
	if err != nil {
		binary = "ghchronicle"
	}
	envPath := filepath.Join(filepath.Dir(configPath), "ghchronicle.env")
	body, start := serviceFile(binary, configPath, envPath, path)
	if mkErr := os.MkdirAll(filepath.Dir(path), configDirMode); mkErr != nil {
		return mkErr
	}
	// The unit itself carries no credential, and systemd reads it as root
	// whatever the account that wrote it: that is what serviceMode is about,
	// and why it is not the 0600 the environment file beside it takes.
	if writeErr := os.WriteFile(path, []byte(body), serviceMode); writeErr != nil {
		return writeErr
	}
	ask.sayf("Wrote %s", path)

	// The credentials go beside the configuration, not inside it, and only the
	// account that runs this can read them.
	if env, hasAny := setupEnvironment(answers); hasAny {
		if writeErr := os.WriteFile(envPath, []byte(env), 0o600); writeErr != nil {
			return writeErr
		}
		ask.sayf("Wrote %s, which holds the credentials.", envPath)
		// Said only where it is true. Go asks the operating system for a mode
		// and Windows does not keep one: the file lands readable by everyone
		// on the machine, and a line claiming otherwise would be worse than
		// no line, because somebody would believe it.
		if runtime.GOOS == "windows" {
			ask.sayf("Windows does not take the permissions this asked for, so restrict it")
			ask.sayf("yourself: right-click, Properties, Security, and remove everyone else.")
		} else {
			ask.sayf("Only you can read it.")
		}
	}
	ask.sayf("")
	ask.sayf("Start it with:")
	ask.sayf("")
	for _, line := range start {
		ask.sayf("  %s", line)
	}
	return nil
}

// serviceFile is the unit, the agent or the task, and the commands that start
// whichever it is.
func serviceFile(binary, configPath, envPath, servicePath string) (body string, start []string) {
	return thisHost().serviceFile(binary, configPath, envPath, servicePath)
}

// serviceFile is the same for a named machine.
func (h host) serviceFile(binary, configPath, envPath, servicePath string) (body string, start []string) {
	switch h.goos {
	case "linux":
		return systemdUnit(binary, configPath, envPath, h), h.systemdCommands()
	case "darwin":
		return launchdPlist(binary, configPath, envPath), []string{
			"launchctl load " + servicePath,
		}
	case "windows":
		return windowsTask(binary, configPath), []string{
			`schtasks /create /tn ghchronicle /xml "` + servicePath + `"`,
		}
	}
	return "", nil
}

// systemdUnit is the unit, with the hardening a collector can take: it reads a
// token, writes one state file and talks to the network, so everything else
// can be taken away from it.
func systemdUnit(binary, configPath, envPath string, h host) string {
	return fmt.Sprintf(`[Unit]
Description=ghchronicle, GitHub metrics collector
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -config %s
EnvironmentFile=%s
Restart=on-failure
RestartSec=30

# It reads a token, writes its state file and talks to GitHub. Nothing else.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=%s
`, binary, configPath, envPath, filepath.Dir(defaultStatePath()), h.systemdTarget())
}

// systemdTarget differs between a machine's own service and an account's.
func (h host) systemdTarget() string {
	if h.root {
		return "multi-user.target"
	}
	return "default.target"
}

// systemdCommands start it, as the machine or as the account.
func (h host) systemdCommands() []string {
	if h.root {
		return []string{
			"systemctl daemon-reload",
			"systemctl enable --now ghchronicle",
			"journalctl -u ghchronicle -f",
		}
	}
	return []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now ghchronicle",
		"loginctl enable-linger $USER   # so it keeps running when you log out",
		"journalctl --user -u ghchronicle -f",
	}
}

// launchdPlist is the agent. KeepAlive rather than RunAtLoad alone, so it comes
// back the way the systemd unit does.
func launchdPlist(binary, configPath, envPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.jmrp.ghchronicle</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>-config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <!-- launchd has no EnvironmentFile, so the credentials are read by the
       shell that starts it. The file is %s and only you can read it. -->
  <key>EnvironmentVariables</key>
  <dict><key>GHCHRONICLE_ENV_FILE</key><string>%s</string></dict>
  <key>StandardOutPath</key><string>/tmp/ghchronicle.log</string>
  <key>StandardErrorPath</key><string>/tmp/ghchronicle.log</string>
</dict>
</plist>
`, binary, configPath, envPath, envPath)
}

// windowsTask is the task, as an XML schtasks takes. Every fifteen minutes
// rather than one long-running process, because that is what a scheduled task
// is for and Windows has no unit file.
func windowsTask(binary, configPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>ghchronicle, GitHub metrics collector</Description>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger><Enabled>true</Enabled></BootTrigger>
  </Triggers>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions>
    <Exec>
      <Command>%s</Command>
      <Arguments>-config "%s"</Arguments>
    </Exec>
  </Actions>
</Task>
`, binary, configPath)
}

// hiddenReader reads a line without echoing it, where the terminal can be told
// to stop echoing. Elsewhere it is nil and the secret is typed in the open,
// which is what a pipe does and is the honest answer for one.
func hiddenReader(in *os.File) func() (string, error) {
	if !interactive(in) {
		return nil
	}
	return func() (string, error) {
		line, err := readHidden(in)
		return strings.TrimSpace(line), err
	}
}
