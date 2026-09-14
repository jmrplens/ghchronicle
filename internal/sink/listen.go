package sink

import (
	"context"
	"net"
)

// listen is split out so the exporter can report a busy port at start-up
// rather than in a goroutine nobody is reading.
//
// The context bounds the bind alone. A TCP listener is created without
// blocking and then outlives this call, so threading a caller's context in
// here would suggest a lifetime it does not have.
func listen(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "tcp", addr)
}
