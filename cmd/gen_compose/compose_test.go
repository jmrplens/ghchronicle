package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What every compose file has to be true of, whichever combination it is.
//
// Bringing them up is the end-to-end suite's job and needs Docker. These are
// the claims the page makes about them, which can be read without one: that a
// reader needs nothing but a token, that no credential is written into the
// file the container gets, and that the collector waits for what the stack
// brought up rather than racing it.

// TestEveryCombinationNeedsNothingButATokenAndAName. The two variables the
// header tells a reader to put in .env are the two the file refuses to start
// without, and there is no third.
func TestEveryCombinationNeedsNothingButATokenAndAName(t *testing.T) {
	t.Parallel()
	for _, c := range Combinations() {
		t.Run(c.File, func(t *testing.T) {
			t.Parallel()
			body := c.Compose()
			required := requiredVariables(body)
			for _, want := range []string{"GITHUB_TOKEN", "GITHUB_USER"} {
				if !required[want] {
					t.Errorf("%s does not refuse to start without %s", c.File, want)
				}
			}
			for name := range required {
				if name != "GITHUB_TOKEN" && name != "GITHUB_USER" {
					t.Errorf("%s also demands %s, which the header does not tell a reader to set",
						c.File, name)
				}
			}
		})
	}
}

// requiredVariables are the ones written with :? , which is compose refusing to
// start rather than substituting a blank.
func requiredVariables(body string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(body, "${")[1:] {
		end := strings.IndexAny(part, ":}")
		if end < 0 || part[end] != ':' || !strings.HasPrefix(part[end:], ":?") {
			continue
		}
		out[part[:end]] = true
	}
	return out
}

// TestNoCredentialIsWrittenIntoTheFileTheContainerGets. Compose interpolates a
// single $ when it builds the config it hands the container, so a token
// written that way ends up inside it; doubled, the reference goes through and
// the collector expands it from its own environment.
func TestNoCredentialIsWrittenIntoTheFileTheContainerGets(t *testing.T) {
	t.Parallel()
	for _, c := range Combinations() {
		t.Run(c.File, func(t *testing.T) {
			t.Parallel()
			body := c.Compose()
			config := body[strings.Index(body, "content: |"):]
			if strings.Contains(config, "token: ${GITHUB_TOKEN}") {
				t.Errorf("%s lets compose put the token into the config it mounts", c.File)
			}
			if !strings.Contains(config, "token: $${GITHUB_TOKEN}") {
				t.Errorf("%s does not pass the token through as a reference", c.File)
			}
		})
	}
}

// TestTheCollectorWaitsForWhatTheStackBrought, so the first sweep does not
// race a database into existence and the publish does not race Grafana.
func TestTheCollectorWaitsForWhatTheStackBrought(t *testing.T) {
	t.Parallel()
	for _, c := range Combinations() {
		t.Run(c.File, func(t *testing.T) {
			t.Parallel()
			body := c.Compose()
			for _, service := range c.waitsFor() {
				if !strings.Contains(body, "      "+service+":\n        condition: service_healthy") {
					t.Errorf("%s does not wait for %s to be healthy", c.File, service)
				}
			}
			if len(c.waitsFor()) == 0 && strings.Contains(body, "depends_on") {
				t.Errorf("%s waits for something it does not bring up", c.File)
			}
		})
	}
}

// TestAStackWithGrafanaPublishesIntoIt, which is the whole promise of the
// checkbox: a reader who ticks it opens Grafana and the dashboard is there.
func TestAStackWithGrafanaPublishesIntoIt(t *testing.T) {
	t.Parallel()
	for _, c := range Combinations() {
		t.Run(c.File, func(t *testing.T) {
			t.Parallel()
			body := c.Compose()
			has := strings.Contains(body, "publish_on_start: true")
			if has != c.Grafana {
				t.Errorf("%s publishes on start = %v, want %v", c.File, has, c.Grafana)
			}
			if !c.Grafana {
				return
			}
			// And with a credential a fresh Grafana actually has, which is the
			// admin password the stack set, not a token nobody made.
			if !strings.Contains(body, "user: admin") {
				t.Errorf("%s brings a Grafana it has no way to log into", c.File)
			}
		})
	}
}

// TestWhatIsCommittedIsWhatTheGeneratorWrites, which is what makes the page's
// promise about them true.
func TestWhatIsCommittedIsWhatTheGeneratorWrites(t *testing.T) {
	t.Parallel()
	for _, c := range Combinations() {
		t.Run(c.File, func(t *testing.T) {
			t.Parallel()
			committed, err := os.ReadFile(filepath.Join("..", "..", "deploy", c.File))
			if err != nil {
				t.Fatalf("%v; run: make compose", err)
			}
			if string(committed) != c.Compose() {
				t.Errorf("%s is not what the generator writes; run: make compose", c.File)
			}
		})
	}
}
