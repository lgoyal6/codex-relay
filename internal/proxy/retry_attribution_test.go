package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

// After a quota refusal the turn is re-sent to a DIFFERENT workspace. Everything the relay
// learns from that second response therefore describes the second workspace, and everything
// it tells the client about who served the turn must name the second workspace too.
//
// The identity used to build the first request is the natural thing to reach for when
// recording the result, and it is the wrong one: it still names the workspace that refused.
// These tests pin the attribution to the workspace that actually served the turn.

// retrySetup builds a proxy whose first upstream call refuses on quota and whose second
// succeeds, returning whatever the caller wants the successful response to look like.
func retrySetup(t *testing.T, onServe func(w http.ResponseWriter)) (*Proxy, *fakeSelector, *httptest.ResponseRecorder, *http.Request, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		mu.Unlock()
		if n == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":"usage limit reached"}`))
			return
		}
		onServe(w)
	}))
	t.Cleanup(up.Close)

	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(req policy.Request) policy.Decision {
		for _, ex := range req.Exclude {
			if ex == "ws-a" {
				return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-b", Primary: policy.ReasonHandoff}
			}
		}
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, ChatGPTAccountID: "acct-" + ws, AccessToken: "tok-" + ws}, nil
	}

	p := newProxy(t, sel, up.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-5.1-codex"}`))
	r.Header.Set("thread-id", "t-retry")
	r.Header.Set("Content-Type", "application/json")
	return p, sel, rec, r, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// R1: quota read off the RETRY response describes ws-b's remaining quota. Filing it under
// ws-a corrupts the state the evaluator routes on: the workspace that refused gets credited
// with the other one's consumption, so it looks freer than it is and keeps being chosen.
func TestRetryAttributesResponseQuotaToTheWorkspaceThatServed(t *testing.T) {
	p, sel, rec, r, calls := retrySetup(t, func(w http.ResponseWriter) {
		h := w.Header()
		h.Set("x-codex-primary-used-percent", "73.5")
		h.Set("x-codex-primary-window-minutes", "300")
		h.Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	})
	p.ServeHTTP(rec, r)

	if got := calls(); got != 2 {
		t.Fatalf("upstream saw %d calls, want 2 (refusal then retry)", got)
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.quota) == 0 {
		t.Fatal("no quota was ingested from the retry response")
	}
	if sel.quotaWS != "ws-b" {
		t.Errorf("quota from the retry was filed under %q, want ws-b (the workspace that served the turn)", sel.quotaWS)
	}
}

// R2: the client is told which workspace served its turn. After a retry that header must
// name ws-b; naming ws-a makes the response header disagree with the ownership record and
// with the credential the bytes were actually produced under.
func TestRetryReportsTheServingWorkspaceToTheClient(t *testing.T) {
	p, _, rec, r, _ := retrySetup(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	})
	p.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("client saw %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Codex-Pool-Workspace"); got != "ws-b" {
		t.Errorf("response names workspace %q, want ws-b (the one that served it)", got)
	}
}

// R3: the same attribution bug through the in-band channel. Real generations carry quota as a
// codex.rate_limits frame rather than a header, so the streaming path must attribute to the
// serving workspace too, not to the identity the first attempt was built from.
func TestRetryAttributesInStreamQuotaToTheServingWorkspace(t *testing.T) {
	p, sel, rec, r, _ := retrySetup(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: codex.rate_limits\n" +
			`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":88,"window_minutes":300}}}` +
			"\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	})
	p.ServeHTTP(rec, r)

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.quota) == 0 {
		t.Fatal("no in-stream quota was ingested from the retry response")
	}
	if sel.quotaWS != "ws-b" {
		t.Errorf("in-stream quota was filed under %q, want ws-b", sel.quotaWS)
	}
}

// R4: a retry that never produces a response must not move the conversation. Ownership is
// what the NEXT turn routes on, so binding it to a workspace that never served this turn
// splits the conversation across accounts -- the exact failure the claim guard exists to
// prevent. The client correctly still sees ws-a's real refusal.
func TestFailedRetryLeavesOwnershipWithTheWorkspaceThatServed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"usage limit reached"}`))
	}))
	defer up.Close()

	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(req policy.Request) policy.Decision {
		for _, ex := range req.Exclude {
			if ex == "ws-a" {
				return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-b", Primary: policy.ReasonHandoff}
			}
		}
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	// ws-b is selectable but its credential cannot be produced, so the retry dies after the
	// point where ownership used to have already been moved.
	sel.identityFn = func(ws string) (Identity, error) {
		if ws == "ws-b" {
			return Identity{}, errors.New("no usable credential for ws-b")
		}
		return Identity{WorkspaceID: ws, ChatGPTAccountID: "acct-" + ws, AccessToken: "tok-" + ws}, nil
	}

	p := newProxy(t, sel, up.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-5.1-codex"}`))
	r.Header.Set("thread-id", "t-failed-retry")
	p.ServeHTTP(rec, r)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client saw %d, want the real 429 from ws-a", rec.Code)
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	for _, rs := range sel.reassigns {
		if strings.HasSuffix(rs, "->ws-b") {
			t.Errorf("conversation was moved to ws-b, which never served a turn: %v", sel.reassigns)
		}
	}
	if len(sel.records) != 1 {
		t.Fatalf("recorded %d activity rows, want one row for ws-a's refusal", len(sel.records))
	}
	if got := sel.records[0].Decision.WorkspaceID; got != "ws-a" {
		t.Errorf("failed retry activity was attributed to %q, want ws-a", got)
	}
}

// R5: the audit row for the refusal describes ws-a, because ws-a produced the 429. The retry
// decision names ws-b and must not be reused for evidence about the request that came before it.
func TestRetryRecordsRefusalAgainstTheWorkspaceThatRefused(t *testing.T) {
	p, sel, rec, r, _ := retrySetup(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	})
	p.ServeHTTP(rec, r)

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.records) < 2 {
		t.Fatalf("recorded %d attempts, want refusal and successful retry", len(sel.records))
	}
	refusal := sel.records[0]
	if refusal.Decision.WorkspaceID != "ws-a" {
		t.Errorf("429 was attributed to %q, want ws-a (the workspace that refused)", refusal.Decision.WorkspaceID)
	}
	if refusal.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("refusal upstream status = %d, want 429", refusal.UpstreamStatus)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// R6: the successful activity row's upstream latency measures ws-b's request. Keeping ws-a's
// faster refusal time makes the history claim the successful upstream accepted sooner than it did.
func TestRetryRecordsTheSuccessfulAttemptUpstreamLatency(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	calls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			fake.Advance(125 * time.Millisecond)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"detail":"usage limit reached"}`)),
			}, nil
		}
		fake.Advance(640 * time.Millisecond)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")),
		}, nil
	})

	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(req policy.Request) policy.Decision {
		if len(req.Exclude) > 0 {
			return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-b", Primary: policy.ReasonHandoff}
		}
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, ChatGPTAccountID: "acct-" + ws, AccessToken: "tok-" + ws}, nil
	}

	p := New(Options{
		UpstreamBase: "https://example.test/backend-api",
		Selector:     sel,
		Clock:        fake,
		HTTPClient:   &http.Client{Transport: transport},
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-5.1-codex"}`))
	r.Header.Set("thread-id", "t-retry-latency")
	p.ServeHTTP(rec, r)

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.records) < 2 {
		t.Fatalf("recorded %d attempts, want refusal and successful retry", len(sel.records))
	}
	if got := sel.records[0].UpstreamMS; got != 125 {
		t.Errorf("refusal upstream_ms = %d, want 125", got)
	}
	if got := sel.records[len(sel.records)-1].UpstreamMS; got != 640 {
		t.Errorf("successful retry upstream_ms = %d, want 640", got)
	}
}
