// Package docker runs the sinks' real stores in containers, so that a test can
// prove a store accepted what a sink emitted rather than that a capture server
// echoed it back.
//
// The suite itself is behind the dockere2e build tag and never runs as part of
// `go test ./...`: it needs Docker, it starts nine containers and it takes
// minutes. `make test-e2e-docker` is the way in, and `make e2e-docker-up`
// leaves the stack running for whoever is looking at a failing assertion.
package docker
