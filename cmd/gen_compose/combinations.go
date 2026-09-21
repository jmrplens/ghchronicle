package main

import (
	"strings"
)

// A combination is one answer to the two questions the documentation asks:
// which store, and whether Grafana comes with it.
//
// Every one of them has to work with nothing but a token in a .env file. That
// is the whole point, and it is why the store is started without a credential
// and Grafana with a known one: a stack that brings up its own store has
// nobody to have created a token in it beforehand, and a reader who has to
// make one before the first `up` is a reader doing the thing this exists to
// avoid.
type Combination struct {
	// Name is what the page's selector calls it.
	Name string
	// File is what it is written to.
	File string
	// Store is the sink's key in the configuration, or "" for a collector on
	// its own.
	Store string
	// Grafana says whether the stack brings one.
	Grafana bool
}

// Combinations is every one the page offers.
func Combinations() []Combination {
	return []Combination{
		{"InfluxDB and Grafana", "compose.influxdb-grafana.yaml", "influxdb", true},
		{"InfluxDB", "compose.influxdb.yaml", "influxdb", false},
		{"PostgreSQL and Grafana", "compose.postgres-grafana.yaml", "postgres", true},
		{"PostgreSQL", "compose.postgres.yaml", "postgres", false},
		{"The collector on its own", "compose.collector.yaml", "", false},
	}
}

// Compose is the file.
func (c Combination) Compose() string {
	var b strings.Builder
	b.WriteString(c.header())
	b.WriteString("\nname: ghchronicle\n\nservices:\n")
	b.WriteString(c.storeService())
	if c.Grafana {
		b.WriteString(grafanaService)
	}
	b.WriteString(c.collectorService())
	b.WriteString("\nconfigs:\n  ghchronicle:\n")
	b.WriteString(configNote)
	b.WriteString(indent(c.config(), "      "))
	b.WriteString(c.volumes())
	return trimmed(b.String())
}

// header says what this is and what the reader has to do, which is one thing.
func (c Combination) header() string {
	var b strings.Builder
	b.WriteString("# ghchronicle, ")
	switch {
	case c.Store == "":
		b.WriteString("on its own.\n")
	case c.Grafana:
		b.WriteString("with " + c.storeName() + " and Grafana.\n")
	default:
		b.WriteString("with " + c.storeName() + ".\n")
	}
	b.WriteString("#\n# Put two lines in a .env file beside this one:\n#\n")
	b.WriteString("#   GITHUB_TOKEN=github_pat_...\n#   GITHUB_USER=your-login\n#\n")
	b.WriteString("# then `docker compose up -d`. ")
	switch {
	case c.Grafana:
		b.WriteString("Grafana is on http://localhost:3000,\n")
		b.WriteString("# admin and the password below, with the dashboard already in it: the\n")
		b.WriteString("# collector publishes it on start and points it at the store beside it.\n")
	case c.Store == "":
		b.WriteString("It writes what it collects to its own\n")
		b.WriteString("# log, which is enough to watch it work. Point sinks at a store of your\n")
		b.WriteString("# own when you have one.\n")
	default:
		b.WriteString("Nothing is published to Grafana;\n")
		b.WriteString("# the store is yours to point one at.\n")
	}
	return b.String()
}

// storeName is what a person calls it.
func (c Combination) storeName() string {
	if c.Store == "postgres" {
		return "PostgreSQL"
	}
	return "InfluxDB"
}
