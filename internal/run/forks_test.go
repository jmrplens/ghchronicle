package run

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestTheAccountIDIsAskedOnceAProcess: a fork's commit history is filtered by
// the account's node id, which the runner asks for the first time a fork
// needs it and keeps for the rest of the process.
func TestTheAccountIDIsAskedOnceAProcess(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	r := sweepRunner(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if req.URL.Path == "/graphql" {
			asked.Add(1)
			_, _ = w.Write([]byte(`{"data":{"user":{"id":"U_kgDOAAAAAQ"}}}`))
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	for range 3 {
		id, err := r.accountID(t.Context())
		if err != nil || id != "U_kgDOAAAAAQ" {
			t.Fatalf("account id = %q, %v", id, err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
	if !strings.Contains(accountIDQuery, "{ id }") {
		t.Errorf("the query asks for more than the id: %s", accountIDQuery)
	}

	// No account to attribute a fork's commits to: nothing is asked.
	r.Cfg.Targets.User, r.accountNodeID = "", ""
	if id, err := r.accountID(t.Context()); id != "" || err != nil || asked.Load() != 1 {
		t.Errorf("without a user: id %q, err %v, asked %d", id, err, asked.Load())
	}
}
