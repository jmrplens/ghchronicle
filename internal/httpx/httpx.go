// Package httpx holds the one piece of HTTP plumbing every client in this
// program needs and the standard library does not give by default.
package httpx

import "net/http"

// OwnTransport returns a transport with a connection pool nobody else holds.
//
// An http.Client built without one uses http.DefaultTransport, which is a
// single pool shared by every client in the process. That has two costs. One
// is real at run time: this program writes to several stores at once, and a
// store that is slow or gone holds connections in a pool the others are
// drawing from. The other shows up in tests, where it is the reason this
// function got a second caller: closing one test's server reaches into that
// shared pool, and a request another test had in flight fails with
// "http: CloseIdleConnections called" rather than with anything about itself.
//
// It is a clone of the standard default so that it keeps the default's proxy
// settings, dial and handshake timeouts. A process that has installed some
// other RoundTripper as http.DefaultTransport, an instrumented or a mocked
// one, has nothing of that kind to clone, and a plain transport that still
// honors the proxy variables is the closest thing to what the clone gives.
func OwnTransport() *http.Transport {
	if standard, ok := http.DefaultTransport.(*http.Transport); ok {
		return standard.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}
