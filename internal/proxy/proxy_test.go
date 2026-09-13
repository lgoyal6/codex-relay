package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

type fakeSelector struct {
	mu         sync.Mutex
	decision   policy.Decision
	identity   Identity
	idErr      error
	owner      string
	claims     []string
	reassigns  []string
	decideFn   func(policy.Request) policy.Decision
	identityFn func(string) (Identity, error)
	claimErr   error
	quota      []upstream.Snapshot
	quotaWS    string
	models     []string
	modelsWS   string
	records    []Record
}

func (f *fakeSelector) Decide(_ context.Context, req policy.Request) policy.Decision {
	if f.decideFn != nil {
		return f.decideFn(req)
	}
	return f.decision
}
func (f *fakeSelector) Identity(_ context.Context, ws string) (Identity, error) {
	if f.identityFn != nil {
		return f.identityFn(ws)
	}
	return f.identity, f.idErr
}
func (f *fakeSelector) OwnerOf(context.Context, string) (string, bool, error) {
	return f.owner, f.owner != "", nil
}
func (f *fakeSelector) Claim(_ context.Context, thread, ws string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, thread+"->"+ws)
	return f.claimErr
}

// Reassign is recorded separately from Claim so a test can tell a handoff from a first bind.
func (f *fakeSelector) Reassign(_ context.Context, thread, ws string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reassigns = append(f.reassigns, thread+"->"+ws)
	return f.claimErr
}
func (f *fakeSelector) ObserveQuota(_ context.Context, ws string, s []upstream.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaWS, f.quota = ws, s
}
func (f *fakeSelector) ObserveModels(_ context.Context, ws string, slugs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelsWS, f.models = ws, slugs
}
func (f *fakeSelector) RecordDecision(r Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
}

func selected(ws string) policy.Decision {
	return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: ws, Primary: policy.ReasonDefault, Summary: "ok"}
}

func newProxy(t *testing.T, sel Selector, upstreamURL string) *Proxy {
	t.Helper()
	return New(Options{
		UpstreamBase: upstreamURL + "/backend-api",
		Selector:     sel,
		Clock:        clock.System(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestGenerationReplacesClientCredentials is the central safety property: whatever identity
// the client sent, the request that reaches the backend carries the SELECTED workspace's
// credential and account id. A merge instead of a replace would bill the wrong workspace.
func TestGenerationReplacesClientCredentials(t *testing.T) {
	var gotAuth, gotAccount, gotPath string
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccount, gotPath = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"), r.URL.Path
		w.Header().Set("x-codex-primary-used-percent", "40")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "TOKEN_WORK"},
	}
	p := newProxy(t, sel, back.URL)

	req := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5.1-codex"}`))
	req.Header.Set("Authorization", "Bearer CLIENT_TOKEN")
	req.Header.Set("ChatGPT-Account-ID", "acct_client")
	req.Header.Set("thread-id", "thread-1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if gotAuth != "Bearer TOKEN_WORK" {
		t.Fatalf("authorization was not replaced: %q", gotAuth)
	}
	if gotAccount != "acct_work" {
		t.Fatalf("account id was not replaced: %q", gotAccount)
	}
	// UpstreamBase already ends in /backend-api, so the path the backend sees is the same
	// one chatgpt.com sees: /backend-api/codex/responses. The local mount point is stripped
	// exactly once, never doubled.
	if gotPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if rec.Header().Get("X-Codex-Pool-Workspace") != "work" {
		t.Fatal("response should name the workspace that served it")
	}
	if len(sel.claims) != 1 || sel.claims[0] != "thread-1->work" {
		t.Fatalf("ownership must be claimed before forwarding, got %v", sel.claims)
	}
	if sel.quotaWS != "work" || len(sel.quota) == 0 || sel.quota[0].Primary.UsedPercent != 40 {
		t.Fatalf("rate-limit headers from the response must be recorded: ws=%q %+v", sel.quotaWS, sel.quota)
	}
}

// TestOwnershipIsClaimedBeforeAnyBytesReachTheClient guards the ordering rule. If the claim
// fails, the client must get a refusal and the upstream must never be called.
func TestOwnershipFailureStopsBeforeUpstream(t *testing.T) {
	called := false
	back := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "a", AccessToken: "t"},
		claimErr: fmt.Errorf("owned by personal"),
	}
	p := newProxy(t, sel, back.URL)
	req := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{}`))
	req.Header.Set("thread-id", "t1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if called {
		t.Fatal("upstream must not be called when ownership cannot be recorded")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestBlockedPolicyNeverReachesUpstream(t *testing.T) {
	called := false
	back := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer back.Close()

	sel := &fakeSelector{decision: policy.Decision{
		Outcome: policy.OutcomeBlocked, Primary: policy.ReasonReserveNoAlternate,
		Summary: "Personal is protected and Work cannot serve this request.",
	}}
	p := newProxy(t, sel, back.URL)
	req := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{}`))
	req.Header.Set("thread-id", "t1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if called {
		t.Fatal("a blocked decision must not spend any quota")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "protected") {
		t.Fatalf("the client must be told why: %s", rec.Body.String())
	}
	if len(sel.records) != 1 || sel.records[0].ErrorClass != "blocked_by_policy" {
		t.Fatalf("a blocked turn must still be recorded in activity: %+v", sel.records)
	}
}

// TestUnknownRouteFailsClosed proves no pooled credential is attached to an unclassified path.
func TestUnknownRouteAttachesNoCredential(t *testing.T) {
	reached := false
	back := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer back.Close()

	sel := &fakeSelector{decision: selected("work"), identity: Identity{AccessToken: "SECRET"}}
	p := newProxy(t, sel, back.URL)
	req := httptest.NewRequest("POST", "/backend-api/wham/billing/charge", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if reached {
		t.Fatal("an unclassified mutation must not be forwarded at all")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("credentials must never appear in an error body")
	}
}

// TestClientOwnedRouteKeepsTheClientCredential: settings and plugins belong to the signed-in
// client, so we must not substitute a pooled token.
func TestClientOwnedRouteIsPassedThroughUntouched(t *testing.T) {
	var gotAuth string
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer back.Close()

	sel := &fakeSelector{decision: selected("work"), identity: Identity{AccessToken: "POOLED"}}
	p := newProxy(t, sel, back.URL)
	req := httptest.NewRequest("GET", "/backend-api/wham/settings/user", nil)
	req.Header.Set("Authorization", "Bearer CLIENT_TOKEN")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotAuth != "Bearer CLIENT_TOKEN" {
		t.Fatalf("client-owned route must keep the client credential, got %q", gotAuth)
	}
}

// TestStreamingIsIncremental proves we do not buffer the whole response before answering.
// A proxy that buffered would make the first token arrive only at the end.
func TestStreamingIsIncremental(t *testing.T) {
	release := make(chan struct{})
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: a\ndata: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: b\ndata: second\n\n")
	}))
	defer back.Close()

	sel := &fakeSelector{decision: selected("w"), identity: Identity{WorkspaceID: "w", AccessToken: "t"}}
	p := newProxy(t, sel, back.URL)

	front := httptest.NewServer(p)
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "t1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	done := make(chan int, 1)
	go func() { n, _ := resp.Body.Read(buf); done <- n }()

	select {
	case n := <-done:
		if !strings.Contains(string(buf[:n]), "first") {
			t.Fatalf("first chunk = %q", string(buf[:n]))
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first chunk did not arrive before the upstream finished: the proxy is buffering")
	}
	close(release)
}

// TestClientCancellationTearsDownUpstream: when Codex cancels a turn, the upstream request
// context must be cancelled too rather than being left to run to completion.
func TestClientCancellationPropagatesUpstream(t *testing.T) {
	gone := make(chan struct{})
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: a\ndata: x\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // upstream observes cancellation
		close(gone)
	}))
	defer back.Close()

	sel := &fakeSelector{decision: selected("w"), identity: Identity{WorkspaceID: "w", AccessToken: "t"}}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "t1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	_, _ = resp.Body.Read(buf)
	cancel()
	resp.Body.Close()

	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the client turn did not tear down the upstream request")
	}
}

func TestThreadIdFallsBackToTurnMetadata(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("x-codex-turn-metadata", `{"thread_id":"from-meta","turn_id":"t"}`)
	if got := conversationID(r); got != "from-meta" {
		t.Fatalf("conversationID = %q", got)
	}
	r.Header.Set("thread-id", "explicit")
	if got := conversationID(r); got != "explicit" {
		t.Fatalf("the explicit thread-id header must win, got %q", got)
	}
}

// TestCancelledTurnIsRecordedAsCancelled closes the reporting gap that made a cancelled turn
// indistinguishable from a completed one: both end on a 200, so only the way the stream
// ended tells them apart. Activity has to be able to explain what happened.
func TestCancelledTurnIsRecordedAsCancelled(t *testing.T) {
	release := make(chan struct{})
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: a\ndata: x\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer back.Close()
	defer close(release)

	sel := &fakeSelector{decision: selected("w"), identity: Identity{WorkspaceID: "w", AccessToken: "t"}}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/backend-api/codex/responses",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "t-cancel")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_, _ = resp.Body.Read(buf)
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sel.mu.Lock()
		n := len(sel.records)
		var class string
		if n > 0 {
			class = sel.records[n-1].ErrorClass
		}
		sel.mu.Unlock()
		if n > 0 {
			if class != "client_cancelled" {
				t.Fatalf("a cancelled turn must be recorded as cancelled, got %q", class)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no activity row was recorded for the cancelled turn")
}

// TestCompletedTurnIsNotMislabelledAsCancelled is the other half: the new classification
// must not turn every normal turn into a cancellation.
func TestCompletedTurnIsNotMislabelledAsCancelled(t *testing.T) {
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	}))
	defer back.Close()

	sel := &fakeSelector{decision: selected("w"), identity: Identity{WorkspaceID: "w", AccessToken: "t"}}
	p := newProxy(t, sel, back.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`)))

	if len(sel.records) != 1 {
		t.Fatalf("expected exactly one activity row, got %d", len(sel.records))
	}
	if sel.records[0].ErrorClass != "" {
		t.Fatalf("a completed turn must carry no error class, got %q", sel.records[0].ErrorClass)
	}
}

// TestCompletedTurnIsNotLabelledCancelledWhenTheClientClosesPromptly pins a regression:
// a client that reads the whole stream and then closes cancels the request context as a
// matter of course. Inferring cancellation from that marked ordinary successful turns as
// cancelled in Activity, which is exactly the kind of misleading state the product must not
// show.
func TestCompletedTurnIsNotLabelledCancelledWhenTheClientClosesPromptly(t *testing.T) {
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: a\ndata: hello\n\n")
		w.(http.Flusher).Flush()
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("w"),
		identity: Identity{WorkspaceID: "w", ChatGPTAccountID: "a", AccessToken: "t"},
	}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "complete-then-close")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close() // read everything, then close promptly, exactly as a real client does

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sel.mu.Lock()
		n := len(sel.records)
		sel.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.records) == 0 {
		t.Fatal("the turn was not recorded")
	}
	if got := sel.records[0].ErrorClass; got == "client_cancelled" {
		t.Fatalf("a turn that completed and was then closed must not be recorded as cancelled (got %q)", got)
	}
}
