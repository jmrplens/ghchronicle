package main

import "strings"

// The services each combination is made of.

// grafanaService is the same wherever it appears. The password is the one
// thing a stack can arrange in advance, and it is what lets the collector
// publish into a Grafana that has existed for ten seconds and has no service
// account in it.
const grafanaService = `
  grafana:
    image: grafana/grafana:12.3.0
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD:-ghchronicle}
    ports:
      - "3000:3000"
    volumes:
      - grafana:/var/lib/grafana
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:3000/api/health || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 30
`

// configNote explains the one piece of syntax a reader would otherwise have to
// look up, and it is worth explaining because getting it wrong writes a token
// into a file.
const configNote = `    # The doubled $$ are deliberate. A single $ is read by compose, which
    # would put the token into the file it hands the container; doubled,
    # compose writes the reference through and the collector expands it from
    # its own environment when it starts.
    content: |
`

// storeService is the database, when the stack brings one.
func (c Combination) storeService() string {
	switch c.Store {
	case "influxdb":
		return `
  influxdb:
    image: influxdb:3-core
    # Without a credential, because a store that exists for the first time
    # when the collector first writes to it has nobody to have made one.
    command:
      - influxdb3
      - serve
      - --node-id=node0
      - --object-store=file
      - --data-dir=/var/lib/influxdb3
      - --without-auth
    volumes:
      - influx:/var/lib/influxdb3
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:8181/health"]
      interval: 5s
      timeout: 3s
      retries: 30
`
	case "postgres":
		return `
  postgres:
    image: postgres:18.6-alpine
    environment:
      POSTGRES_USER: ghchronicle
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-ghchronicle}
      POSTGRES_DB: ghchronicle
    volumes:
      # /var/lib/postgresql, not /var/lib/postgresql/data: the 18+ images
      # moved where they keep the cluster, and a volume on the old path is
      # refused outright with "in 18+, these Docker images are configured to
      # store database data in a subdirectory".
      - postgres:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ghchronicle"]
      interval: 5s
      timeout: 3s
      retries: 30
`
	default:
		return ""
	}
}

// collectorService waits for whatever the stack brought up, so the first sweep
// does not race the store into existence.
func (c Combination) collectorService() string {
	var b strings.Builder
	b.WriteString(`
  ghchronicle:
    image: ghcr.io/jmrplens/ghchronicle
    command: ["-config", "/config.yaml"]
    restart: unless-stopped
`)
	if waits := c.waitsFor(); len(waits) > 0 {
		b.WriteString("    depends_on:\n")
		for _, name := range waits {
			b.WriteString("      " + name + ":\n        condition: service_healthy\n")
		}
	}
	b.WriteString("    environment:\n")
	b.WriteString("      GITHUB_TOKEN: ${GITHUB_TOKEN:?put your token in a .env file beside this}\n")
	if c.Grafana {
		b.WriteString("      GRAFANA_PASSWORD: ${GRAFANA_PASSWORD:-ghchronicle}\n")
	}
	if c.Store == "postgres" {
		b.WriteString("      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-ghchronicle}\n")
	}
	b.WriteString(`    volumes:
      - state:/var/lib/ghchronicle
    configs:
      - source: ghchronicle
        target: /config.yaml
`)
	return b.String()
}

// waitsFor is what has to be healthy first.
func (c Combination) waitsFor() []string {
	var out []string
	if c.Store != "" {
		out = append(out, c.Store)
	}
	if c.Grafana {
		out = append(out, "grafana")
	}
	return out
}

// volumes is one per thing that keeps something.
func (c Combination) volumes() string {
	names := []string{"state"}
	switch c.Store {
	case "influxdb":
		names = append([]string{"influx"}, names...)
	case "postgres":
		names = append([]string{"postgres"}, names...)
	}
	if c.Grafana {
		names = append(names, "grafana")
	}
	var b strings.Builder
	b.WriteString("\nvolumes:\n")
	for _, name := range names {
		b.WriteString("  " + name + ":\n")
	}
	return b.String()
}

// config is what the collector reads, which is the same file the guided setup
// would have written for the same answers.
func (c Combination) config() string {
	var b strings.Builder
	b.WriteString("github:\n  token: $${GITHUB_TOKEN}\ntargets:\n")
	b.WriteString("  user: ${GITHUB_USER:?put the account to collect in a .env file beside this}\n")
	b.WriteString("sinks:\n")
	switch c.Store {
	case "influxdb":
		b.WriteString("  influxdb:\n    url: http://influxdb:8181\n    bucket: github\n")
	case "postgres":
		b.WriteString("  postgres:\n    dsn: postgres://ghchronicle:$${POSTGRES_PASSWORD}" +
			"@postgres:5432/ghchronicle?sslmode=disable\n")
	default:
		b.WriteString("  stdout: true\n")
	}
	if c.Grafana {
		b.WriteString("grafana:\n  url: http://grafana:3000\n  user: admin\n")
		b.WriteString("  password: $${GRAFANA_PASSWORD}\n  publish_on_start: true\n")
		if c.Store == "postgres" {
			// Grafana's PostgreSQL datasource has no mode for libpq's default,
			// and this stack speaks plaintext on a private network.
			b.WriteString("  datasource:\n    sslmode: disable\n")
		}
	}
	b.WriteString("state_file: /var/lib/ghchronicle/state.json\n")
	return b.String()
}

// indent puts a block under a YAML key.
func indent(body, prefix string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}
