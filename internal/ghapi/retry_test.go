package ghapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/httpx"
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

// askedAgainStatuses are the answers send asks once more for: GitHub failing
// to finish the answer, at its gateway or in the application.
var askedAgainStatuses = []int{
	http.StatusInternalServerError, http.StatusBadGateway,
	http.StatusServiceUnavailable, http.StatusGatewayTimeout,
}

// TestAFailureToFinishIsAskedOnceMore is issue #89 and its sequel: the
// artifact listing of two repositories answered 502 after ten seconds on 15
// of 54 passes, one of them later 500 after eight, and a pass that met either
// lost the repository's storage total. A second attempt is what the next pass
// got, so it is what this one gets.
func TestAFailureToFinishIsAskedOnceMore(t *testing.T) {
	t.Parallel()
	for _, status := range askedAgainStatuses {
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

// TestTwoFailuresInARowReturnTheStatus: once, not until it works. The second
// answer is the one returned, with its status and its body.
func TestTwoFailuresInARowReturnTheStatus(t *testing.T) {
	t.Parallel()
	for _, status := range askedAgainStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c, calls := failingFirst(t, 2, status, answerJSON(`{}`))
			_, _, err := c.GetJSON(context.Background(), "/repos/o/n/actions/artifacts?per_page=100&page=2", &struct{}{}, "")
			se, ok := errors.AsType[*StatusError](err)
			if !ok || se.Code != status || se.Body != `{"message":"Server Error"}` {
				t.Fatalf("err = %v, want the second %d as a StatusError with its body", err, status)
			}
			if se.Path != "/repos/o/n/actions/artifacts?per_page=100&page=2" {
				t.Errorf("Path = %q, want the request", se.Path)
			}
			if n := calls.Load(); n != 2 {
				t.Errorf("the server saw %d requests, want exactly two", n)
			}
		})
	}
}

// TestARefusalIsNotAskedAgain: a 4xx is the same answer asked again, and so
// is a 5xx that says the request itself will never be served. Only the four
// failures to finish an answer are asked again, not every status from 500.
func TestARefusalIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusConflict, http.StatusGone,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusNotImplemented, http.StatusHTTPVersionNotSupported,
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

// TestTheRetryIsBraked: a 500 and a 502 are charged, and the brake reads
// their headers like any other answer's. A budget the failure left at the
// reserve is not spent on asking again.
func TestTheRetryIsBraked(t *testing.T) {
	t.Parallel()
	for _, status := range askedAgainStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				rateHeaders(w, "core", 5000, 10, time.Now().Add(time.Hour))
				w.WriteHeader(status)
			})
			c.SetReserve(500, false)
			_, _, err := c.GetJSON(context.Background(), "/p", &struct{}{}, "")
			if _, ok := errors.AsType[*RateLimitedError](err); !ok {
				t.Errorf("err = %v, want the brake's refusal", err)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the server saw %d requests, want only the one that spent the budget", n)
			}
		})
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

// TestGraphQLIsNotAskedAgainByTheClient: a query is a POST, and the
// gateway's 502 on one is a query too large, which the collectors ask again
// with a smaller page. Asking the same one again here would be ten more
// seconds for the same answer.
func TestGraphQLIsNotAskedAgainByTheClient(t *testing.T) {
	t.Parallel()
	for _, status := range askedAgainStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(status)
			})
			if _, ok := errors.AsType[*TooLargeError](c.GraphQL(context.Background(), "{}", nil, nil)); !ok {
				t.Errorf("a %d on a query is no longer a TooLargeError", status)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("the server saw %d queries, want one", n)
			}
		})
	}
}

// roundTrip is a transport made of a function, for a failure no server can
// be made to give on demand, and for counting what the client sent.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// counted wraps a transport so a test can see how many requests the client
// made through it, including the ones that never reached a server.
func counted(inner http.RoundTripper) (http.RoundTripper, *atomic.Int32) {
	var n atomic.Int32
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return inner.RoundTrip(r)
	}), &n
}

// lookupFailed is the error a DNS lookup that could not be made comes back
// as, shaped as the job log's download met it on 2026-09-28: net/http wraps
// it in a *url.Error on the way out, as it would a real one.
func lookupFailed(host string) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
		Err:  "dial udp [fe80::1%enp1s0]:53: connect: invalid argument",
		Name: host, Server: "[fe80::1%enp1s0]:53",
	}}
}

// networkFailure is one way a request can get no answer at all: the base URL
// a client is pointed at, the transport it goes through, how many
// connections the far end accepted, and what the error must carry.
type networkFailure struct {
	name     string
	setup    func(t *testing.T) (base string, rt http.RoundTripper, accepted *atomic.Int32)
	carrying func(error) bool
}

// networkFailures are made for real where a local socket can make them, and
// injected where only a resolver could: a lookup is the one kind production
// met, and a test cannot make the host's resolver fail.
func networkFailures() []networkFailure {
	return []networkFailure{
		{
			name: "a DNS lookup that failed",
			setup: func(*testing.T) (string, http.RoundTripper, *atomic.Int32) {
				return "http://api.example.test", roundTrip(func(r *http.Request) (*http.Response, error) {
					return nil, lookupFailed(r.URL.Hostname())
				}), &atomic.Int32{}
			},
			carrying: func(err error) bool { _, ok := errors.AsType[*net.DNSError](err); return ok },
		},
		{
			name: "a connection refused",
			setup: func(t *testing.T) (string, http.RoundTripper, *atomic.Int32) {
				t.Helper()
				ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				return "http://" + addr, httpx.OwnTransport(), &atomic.Int32{}
			},
			carrying: func(err error) bool {
				op, ok := errors.AsType[*net.OpError](err)
				return ok && op.Op == "dial"
			},
		},
		{
			name: "a connection reset",
			setup: func(t *testing.T) (string, http.RoundTripper, *atomic.Int32) {
				t.Helper()
				accepted := serve(t, func(conn net.Conn) {
					// Read the request, then close with no linger, which
					// sends a reset rather than an orderly end.
					_, _ = conn.Read(make([]byte, 4096))
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
					_ = conn.Close()
				})
				return "http://" + accepted.addr, httpx.OwnTransport(), &accepted.n
			},
			carrying: func(err error) bool {
				return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
			},
		},
		{
			name: "a TLS handshake that timed out",
			setup: func(t *testing.T) (string, http.RoundTripper, *atomic.Int32) {
				t.Helper()
				accepted := serve(t, func(conn net.Conn) {
					// Say nothing until the test is over.
					<-t.Context().Done()
					_ = conn.Close()
				})
				rt := httpx.OwnTransport()
				rt.TLSHandshakeTimeout = 100 * time.Millisecond
				return "https://" + accepted.addr, rt, &accepted.n
			},
			carrying: func(err error) bool { return err != nil && strings.Contains(err.Error(), "TLS handshake timeout") },
		},
	}
}

// listening is a raw TCP server whose answers a test decides, and how
// many connections it has accepted.
type listening struct {
	addr string
	n    atomic.Int32
}

func serve(t *testing.T, handle func(net.Conn)) *listening {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	l := &listening{addr: ln.Addr().String()}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			l.n.Add(1)
			go handle(conn)
		}
	}()
	return l
}

// TestANetworkFailureIsNotAskedAgain: a request that got no answer at all
// is left to the next pass. The lookups measured had already been asked
// twice by the resolver when they failed, and the other three kinds never
// appeared in 457,098 requests; see send.
func TestANetworkFailureIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	asks := map[string]func(*Client) error{
		"GetJSON": func(c *Client) error {
			_, _, err := c.GetJSON(context.Background(), "/repos/o/n/actions/artifacts", &struct{}{}, "")
			return err
		},
		"GetText": func(c *Client) error {
			_, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
			return err
		},
	}
	for _, failure := range networkFailures() {
		for how, ask := range asks {
			t.Run(failure.name+"/"+how, func(t *testing.T) {
				t.Parallel()
				base, rt, accepted := failure.setup(t)
				c := New("test-token", 5*time.Second)
				c.SetBaseURL(base)
				c.SetRetryPause(0)
				var sent *atomic.Int32
				c.http.Transport, sent = counted(rt)
				err := ask(c)
				t.Log(err)
				if !failure.carrying(err) {
					t.Fatalf("err = %v, want %s", err, failure.name)
				}
				if n := sent.Load(); n != 1 {
					t.Errorf("the client sent %d requests, want one", n)
				}
				if n := accepted.Load(); n > 1 {
					t.Errorf("the far end accepted %d connections, want at most one", n)
				}
			})
		}
	}
}

// jobLog is an API that redirects a job log to a storage server of its own,
// on another host as GitHub's does, whose answers the test decides. It counts
// what each side was asked.
type jobLog struct {
	client            *Client
	api, storage      atomic.Int32
	storageAuthorized atomic.Int32
}

func newJobLog(t *testing.T, apiRemaining int, storage http.HandlerFunc) *jobLog {
	t.Helper()
	j := &jobLog{}
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.storage.Add(1)
		if r.Header.Get("Authorization") != "" {
			j.storageAuthorized.Add(1)
		}
		if r.URL.Query().Get("sig") != "signed" {
			t.Errorf("the signature was lost on the way to storage: %s", r.URL.RawQuery)
		}
		storage(w, r)
	}))
	t.Cleanup(blob.Close)
	j.client, _ = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		j.api.Add(1)
		rateHeaders(w, "core", 5000, apiRemaining, time.Now().Add(time.Hour))
		http.Redirect(w, r, blob.URL+"/actions-results/job-logs.txt?sig=signed", http.StatusFound)
	})
	return j
}

// failingStorage answers the first n requests with status and then the log.
func failingStorage(n int32, status int) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= n {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("<Error><Code>ServerBusy</Code></Error>"))
			return
		}
		_, _ = w.Write([]byte("2026-09-28T07:11:40Z ##[error]Process completed with exit code 1.\n"))
	}
}

// TestStorageIsAskedAgainOnItsOwn: the redirect was the API's answer and was
// charged; what failed is the object storage it named. The retry goes to
// storage alone, on the same signed URL, rather than asking the API for a new
// redirect, which is a core request.
func TestStorageIsAskedAgainOnItsOwn(t *testing.T) {
	t.Parallel()
	for _, status := range askedAgainStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			j := newJobLog(t, 4000, failingStorage(1, status))
			text, err := j.client.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
			if err != nil {
				t.Fatalf("a %d from storage followed by the log failed the request: %v", status, err)
			}
			if !strings.Contains(text, "exit code 1") {
				t.Errorf("text = %q, want the retry's log", text)
			}
			if n := j.api.Load(); n != 1 {
				t.Errorf("the API was asked %d times, want once: its redirect was not what failed", n)
			}
			if n := j.storage.Load(); n != 2 {
				t.Errorf("storage was asked %d times, want the failed request and one more", n)
			}
			if n := j.storageAuthorized.Load(); n != 0 {
				t.Errorf("%d requests to storage carried the token", n)
			}
		})
	}
}

// TestStorageIsNotBraked: storage spends no budget, so a budget the redirect
// left at the reserve does not stop the retry to storage. It did when the
// retry went back through the API.
func TestStorageIsNotBraked(t *testing.T) {
	t.Parallel()
	j := newJobLog(t, 10, failingStorage(1, http.StatusBadGateway))
	j.client.SetReserve(500, false)
	if _, err := j.client.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs"); err != nil {
		t.Fatalf("a budget at the reserve stopped the retry to storage: %v", err)
	}
	if a, s := j.api.Load(), j.storage.Load(); a != 1 || s != 2 {
		t.Errorf("the API was asked %d times and storage %d, want 1 and 2", a, s)
	}
}

// TestStorageTwiceReturnsTheStatus: once, as for the API, and the error names
// the job log that was asked for.
func TestStorageTwiceReturnsTheStatus(t *testing.T) {
	t.Parallel()
	j := newJobLog(t, 4000, failingStorage(2, http.StatusServiceUnavailable))
	_, err := j.client.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusServiceUnavailable || se.Path != "/repos/o/n/actions/jobs/1/logs" {
		t.Fatalf("err = %v, want the second 503 as a StatusError naming the log", err)
	}
	if a, s := j.api.Load(), j.storage.Load(); a != 1 || s != 2 {
		t.Errorf("the API was asked %d times and storage %d, want 1 and 2", a, s)
	}
}

// TestAStorageRefusalIsNotAskedAgain: a signature that expired or a log that
// is gone is the same answer asked again.
func TestAStorageRefusalIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			j := newJobLog(t, 4000, failingStorage(1, status))
			_, err := j.client.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
			if _, ok := errors.AsType[*UnavailableError](err); !ok {
				t.Errorf("err = %v, want the log unavailable", err)
			}
			if a, s := j.api.Load(), j.storage.Load(); a != 1 || s != 1 {
				t.Errorf("the API was asked %d times and storage %d, want once each", a, s)
			}
		})
	}
}

// TestAStorageLookupThatFailedIsNotAskedAgain is the failure of 2026-09-28:
// the API answered with its redirect and the storage host's name could not
// be looked up. Neither side is asked again.
func TestAStorageLookupThatFailedIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	var api, storage atomic.Int32
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		api.Add(1)
		http.Redirect(w, r, "https://productionresultssa14.blob.core.windows.net/job-logs.txt?sig=signed", http.StatusFound)
	})
	inner := srv.Client().Transport
	c.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "productionresultssa14.blob.core.windows.net" {
			storage.Add(1)
			return nil, lookupFailed(r.URL.Hostname())
		}
		return inner.RoundTrip(r)
	})
	_, err := c.GetText(context.Background(), "/repos/o/n/actions/jobs/1/logs")
	if _, ok := errors.AsType[*net.DNSError](err); !ok {
		t.Fatalf("err = %v, want the lookup's failure", err)
	}
	if a, s := api.Load(), storage.Load(); a != 1 || s != 1 {
		t.Errorf("the API was asked %d times and storage %d, want once each", a, s)
	}
}
