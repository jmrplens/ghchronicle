package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// What the guided setup writes, and where.

// configDirMode is what the directory holding a configuration is created with.
// The credentials live beside the file, so nobody else has any reason to walk
// it, and gosec is right to ask.
const configDirMode = 0o750

// probe asks whether something is listening, with a short patience: this is a
// courtesy check inside a conversation, and a person waiting thirty seconds
// for it would rather it had not asked.
func probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return nil
}

// defaultConfigPath is where a configuration belongs on this system, and
// whether this run can write there. Running as root means the machine's own
// place; anything else means the account's, because a setup that needs sudo to
// answer its last question is one that fails at the end.
func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "ghchronicle", "config.yaml")
		}
	}
	if os.Geteuid() == 0 {
		return "/etc/ghchronicle/config.yaml"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "ghchronicle", "config.yaml")
	}
	return "config.yaml"
}

// defaultStatePath is where what a sweep remembers belongs, beside the
// configuration for the same reason.
func defaultStatePath() string {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return "/var/lib/ghchronicle/state.json"
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "state.json"
	}
	return filepath.Join(dir, "ghchronicle", "state.json")
}

// setupConfig is the file the answers come to. Written by hand rather than
// marshaled, because what a reader opens afterwards should read like the
// examples in the documentation: a comment above anything that is not obvious,
// and a credential that is a reference rather than a value.
func setupConfig(a setupAnswers, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `ghchronicle -setup` on %s.\n", now.Format("2006-01-02"))
	b.WriteString("# Every ${VAR} is read from the environment at start-up, so this file\n")
	b.WriteString("# carries no credential and can be copied and kept anywhere.\n\n")
	b.WriteString("github:\n  token: ${GITHUB_TOKEN}\n\n")
	fmt.Fprintf(&b, "targets:\n  user: %s\n\n", a.User)
	b.WriteString("sinks:\n")
	switch a.Sink {
	case "influxdb":
		fmt.Fprintf(&b, "  influxdb:\n    url: %s\n    bucket: %s\n    token: ${INFLUX_TOKEN}\n",
			a.SinkURL, a.Bucket)
	case "postgres":
		b.WriteString("  postgres:\n    dsn: ${DATABASE_URL}\n")
	case "elasticsearch":
		fmt.Fprintf(&b, "  elasticsearch:\n    url: %s\n    prefix: ghchronicle-\n", a.SinkURL)
		if a.SinkKey != "" {
			b.WriteString("    api_key: ${ES_API_KEY}\n")
		}
	case "file":
		fmt.Fprintf(&b, "  file:\n    path: %s\n", a.SinkURL)
	default:
		b.WriteString("  stdout: true\n")
	}
	if a.Dashboard {
		fmt.Fprintf(&b, "\n# The dashboard is published when the collector starts, so a new\n"+
			"# version brings its dashboard with it.\ngrafana:\n  url: %s\n"+
			"  token: ${GRAFANA_TOKEN}\n  publish_on_start: true\n", a.GrafanaURL)
	}
	fmt.Fprintf(&b, "\nstate_file: %s\n", defaultStatePath())
	return b.String()
}

// setupEnvironment is the credentials the configuration refers to, which do not
// go in it. The guided setup writes them beside it, readable by nobody else.
func setupEnvironment(a setupAnswers) (string, bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "GITHUB_TOKEN=%s\n", a.Token)
	if a.Sink == "influxdb" && a.SinkKey != "" {
		fmt.Fprintf(&b, "INFLUX_TOKEN=%s\n", a.SinkKey)
	}
	if a.Sink == "postgres" {
		fmt.Fprintf(&b, "DATABASE_URL=%s\n", a.SinkURL)
	}
	if a.Sink == "elasticsearch" && a.SinkKey != "" {
		fmt.Fprintf(&b, "ES_API_KEY=%s\n", a.SinkKey)
	}
	if a.Dashboard && a.GrafanaToken != "" {
		fmt.Fprintf(&b, "GRAFANA_TOKEN=%s\n", a.GrafanaToken)
	}
	return b.String(), b.Len() > 0
}
