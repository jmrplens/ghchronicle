package ghapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// failingFirst answers the first n requests with status and every later one
// with the handler given, and counts what it saw.
func failingFirst(t *testing.T, n int32, status int, then http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			rateHeaders(w, "core", 5000, 4000, time.Now().Add(time.Hour))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		then(w, r)
	})
	return c, &calls
}

func answerJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rateHeaders(w, "core", 5000, 3999, time.Now().Add(time.Hour))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// TestAGatewayErrorIsAskedOnceMore is issue #89: the artifact listing of two
// repositories answered 502 after ten seconds on 15 of 54 passes, and a pass
// that met one lost the repository's storage total. A second attempt is what
// the next pass got, so it is what this one gets.
func TestAGatewayErrorIsAskedOnceMore(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c, calls := failingFirst(t, 1, status, answerJSON(`{"total_count": 29405}`))
			var out struct {
				Total int `json:"total_count"`
			}
			if _, _, err := c.GetJSON(context.Background(), "/repos/o/n/actions/artifacts?per_page=100&page=2", &out, ""); err != nil {
				t.Fatalf("a %d followed by a 200 failed the request: %v", status, err)
			}
			if out.Total != 29405 {
				t.Errorf("decoded %+v, want the retry's body", out)
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("the server saw %d requests, want the failed one and one more", n)
			}
			// The budget is the retry's, the newest GitHub reported.
			if r, _ := c.RateFor("core"); r.Remaining != 3999 {
				t.Errorf("remaining = %d, want the retry's 3999", r.Remaining)
			}
		})
	}
}

// TestTwoGatewayErrorsInARowReturnTheStatus: once, not until it works. The
// second answer is the one returned, with its status and its body.
func TestTwoGatewayErrorsInARowReturnTheStatus(t *testing.T) {
	t.Parallel()
	c, calls := failingFirst(t, 2, http.StatusBadGateway, answerJSON(`{}`))
	_, _, err := c.GetJSON(context.Background(), "/repos/o/n/actions/artifacts?per_page=100&page=2", &struct{}{}, "")
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusBadGateway || se.Body != `{"message":"Server Error"}` {
		t.Fatalf("err = %v, want the second 502 as a StatusError with its body", err)
	}
	if se.Path != "/repos/o/n/actions/artifacts?per_page=100&page=2" {
		t.Errorf("Path = %q, want the request", se.Path)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want exactly two", n)
	}
}

// TestOnlyTheGatewayStatusesAreAskedAgain: a 500 or a 503 is not a gateway
// that ran out of time, and neither is a refusal or a spent budget.
func TestOnlyTheGatewayStatusesAreAskedAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusServiceUnavailable,
		http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c, calls := failingFirst(t, 1, status, answerJSON(`{}`))
			if _, _, err := c.GetJSON(context.Background(), "/repos/o/n/x", &struct{}{}, ""); err == nil {
				t.Errorf("a %d was answered by asking again", status)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the server saw %d requests for a %d, want one", n, status)
			}
		})
	}
}

// TestTheRetryAsksWithTheSameValidator: the ETag and the body read before the
// first attempt are still a pair when the second is sent, so the retry is
// conditional too, and a 304 to it replays the stored body at no cost.
func TestTheRetryAsksWithTheSameValidator(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Link", `<https://api.github.com/x?page=3>; rel="next"`)
			_, _ = w.Write([]byte(`{"total_count": 153}`))
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"v1"` {
				t.Errorf("the first attempt asked with %q", got)
			}
			w.WriteHeader(http.StatusBadGateway)
		default:
			if got := r.Header.Get("If-None-Match"); got != `"v1"` {
				t.Errorf("the retry asked with %q, want the stored ETag", got)
			}
			w.WriteHeader(http.StatusNotModified)
		}
	})
	type page struct {
		Total int `json:"total_count"`
	}
	var first, second page
	if _, _, err := c.GetJSON(context.Background(), "/p", &first, ""); err != nil {
		t.Fatal(err)
	}
	link, cached, err := c.GetJSON(context.Background(), "/p", &second, "")
	if err != nil {
		t.Fatalf("a 502 then a 304 failed the request: %v", err)
	}
	if !cached || second.Total != 153 {
		t.Errorf("cached=%v body=%+v, want the stored body replayed", cached, second)
	}
	if link != `<https://api.github.com/x?page=3>; rel="next"` {
		t.Errorf("link = %q, want the one stored with the body", link)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("the server saw %d requests, want three", n)
	}
}

// TestTheRetryIsBraked: a 502 is charged, and the brake reads its headers
// like any other answer's. A budget the 502 left at the reserve is not spent
// on asking again.
func TestTheRetryIsBraked(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		rateHeaders(w, "core", 5000, 10, time.Now().Add(time.Hour))
		w.WriteHeader(http.StatusBadGateway)
	})
	c.SetReserve(500, false)
	_, _, err := c.GetJSON(context.Background(), "/p", &struct{}{}, "")
	if _, ok := errors.AsType[*RateLimitedError](err); !ok {
		t.Errorf("err = %v, want the brake's refusal", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want only the one that spent the budget", n)
	}
}

// TestTheRetryWaitsThePause: the retry is not sent back to back with the
// answer that failed.
func TestTheRetryWaitsThePause(t *testing.T) {
	t.Parallel()
	var failedAt, retriedAt atomic.Int64
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			failedAt.Store(time.Now().UnixNano())
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		retriedAt.Store(time.Now().UnixNano())
		_, _ = w.Write([]byte(`{}`))
	})
	const pause = 150 * time.Millisecond
	c.SetRetryPause(pause)
	if _, _, err := c.GetJSON(context.Background(), "/p", &struct{}{}, ""); err != nil {
		t.Fatal(err)
	}
	if gap := time.Duration(retriedAt.Load() - failedAt.Load()); gap < pause {
		t.Errorf("the retry came %v after the 504, want at least %v", gap, pause)
	}
	if New("", 0).retryPause != DefaultRetryPause {
		t.Error("a new client does not pause for the default")
	}
}

// TestACancelDuringThePauseAsksNothingMore: a sweep being shut down does not
// wait out the pause, and does not send the retry. Both halves of the error
// are there to be asked for: the status for what GitHub said, the
// cancellation for why nothing followed.
func TestACancelDuringThePauseAsksNothingMore(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The answer is handed over by hand rather than by a server, so the
	// cancellation lands after the 502 has arrived and not while the
	// transport is still waiting for it, which would be a different error.
	var calls atomic.Int32
	do := func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return &http.Response{
			StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway",
			Header: http.Header{}, Body: http.NoBody,
		}, nil
	}
	c := New("test-token", 0)
	c.SetRetryPause(time.Hour)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/p", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := c.send(ctx, "/p", req, do)
	if resp != nil {
		_ = resp.Body.Close()
		t.Errorf("a response came back: %d", resp.StatusCode)
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("returned after %v, the pause was waited out", took)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry the cancellation", err)
	}
	if se, ok := errors.AsType[*StatusError](err); !ok || se.Code != http.StatusBadGateway {
		t.Errorf("err = %v, want it to carry the 502", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want one", n)
	}
}

// TestGetTextAsksAGatewayErrorOnceMore: the job log and the bare commit SHA
// are REST GETs as well, behind the same gateway.
func TestGetTextAsksAGatewayErrorOnceMore(t *testing.T) {
	t.Parallel()
	c, calls := failingFirst(t, 1, http.StatusBadGateway, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789abcdef"))
	})
	got, err := c.GetTextAs(context.Background(), "/repos/o/n/commits/main", "application/vnd.github.sha")
	if err != nil || got != "0123456789abcdef" {
		t.Errorf("got %q, %v; want the retry's answer", got, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want two", n)
	}
}

// TestGraphQLIsNotAskedAgainByTheClient: the gateway's 502 on a query is a
// query too large, and the collectors ask again with a smaller page. Asking
// the same one again here would be ten more seconds for the same answer.
func TestGraphQLIsNotAskedAgainByTheClient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, ok := errors.AsType[*TooLargeError](c.GraphQL(context.Background(), "{}", nil, nil)); !ok {
		t.Error("a 502 on a query is no longer a TooLargeError")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the server saw %d queries, want one", n)
	}
}
