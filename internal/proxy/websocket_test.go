package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

// TestWebSocketBridgeReplacesCredentialsAndCarriesFrames exercises the transport Codex tries
// first. It is a real upgrade against a real WebSocket upstream, not a simulated one: the
// unit under test is the bridge, and a bridge that only works in theory is worth nothing.
func TestWebSocketBridgeReplacesCredentialsAndCarriesFrames(t *testing.T) {
	var gotAuth, gotAccount, gotPath string
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		gotPath = r.URL.Path
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("upstream accept: %v", err)
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		_ = c.Write(ctx, typ, append([]byte("echo:"), data...))
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "TOKEN_WORK"},
	}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer CLIENT_TOKEN")
	hdr.Set("ChatGPT-Account-ID", "acct_client")
	hdr.Set("thread-id", "thread-ws")
	c, _, err := websocket.Dial(ctx, toWebSocketURL(front.URL)+"/backend-api/codex/responses",
		&websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("client dial through the proxy failed: %v", err)
	}
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("frame did not round trip: %q", got)
	}
	if gotAuth != "Bearer TOKEN_WORK" {
		t.Fatalf("the upgrade must carry the selected workspace's token, got %q", gotAuth)
	}
	if gotAccount != "acct_work" {
		t.Fatalf("the upgrade must carry the selected workspace's account id, got %q", gotAccount)
	}
	if gotPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if len(sel.claims) != 1 || sel.claims[0] != "thread-ws->work" {
		t.Fatalf("the websocket path must claim ownership too, got %v", sel.claims)
	}
}

// TestWebSocketUpgradeRefusedWhenPolicyBlocks proves the blocked case is a real HTTP status
// the client can act on, and that upstream is never dialled.
func TestWebSocketUpgradeRefusedWhenPolicyBlocks(t *testing.T) {
	dialled := false
	back := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialled = true }))
	defer back.Close()

	sel := &fakeSelector{decision: policy.Decision{
		Outcome: policy.OutcomeBlocked,
		Primary: policy.ReasonReserveNoAlternate,
		Summary: "Personal is protected and no alternative can serve this request.",
	}}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, toWebSocketURL(front.URL)+"/backend-api/codex/responses", nil)
	if err == nil {
		t.Fatal("the upgrade must be refused while policy blocks the request")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 so the client can surface the reason, got %v", resp)
	}
	if dialled {
		t.Fatal("a blocked request must never reach upstream")
	}
	if len(sel.records) == 0 {
		t.Fatal("a blocked upgrade should still be recorded in activity")
	}
	if !strings.Contains(sel.records[0].Decision.Summary, "protected") {
		t.Fatalf("the recorded reason should be the policy's own wording, got %q", sel.records[0].Decision.Summary)
	}
}

// TestFailedUpgradeDoesNotBindTheConversation is the reason ownership is claimed after the
// sockets exist rather than before the dial.
//
// Codex attempts a WebSocket upgrade first and falls back to HTTP SSE when it fails. If a
// refused upgrade bound the conversation, the fallback request would arrive already
// constrained to a workspace that never served a single turn.
func TestFailedUpgradeDoesNotBindTheConversation(t *testing.T) {
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An upstream that does not speak WebSocket on this route, which is exactly what a
		// non-WebSocket backend does.
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "T"},
	}
	front := httptest.NewServer(newProxy(t, sel, back.URL))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("thread-id", "thread-fallback")
	_, resp, err := websocket.Dial(ctx, toWebSocketURL(front.URL)+"/backend-api/codex/responses",
		&websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		t.Fatal("the upgrade should have failed")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the client needs the upstream status to decide to fall back, got %v", resp)
	}
	if len(sel.claims) != 0 {
		t.Fatalf("a refused upgrade must not bind the conversation, got %v", sel.claims)
	}
	if len(sel.records) != 1 || sel.records[0].ErrorClass != "upstream_upgrade_refused" {
		t.Fatalf("the refused upgrade should be recorded as such, got %+v", sel.records)
	}
}

// TestWebSocketUpgradeIngestsQuotaHeaders pins the defect a real-backend run exposed: real
// turns are WebSocket upgrades, quota rides on the upgrade response, and this path recorded
// none of it. Every earlier test passed because the mock upstream refused the upgrade and
// Codex fell back to HTTP SSE, where ingestion already worked.
func TestWebSocketUpgradeIngestsQuotaHeaders(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real upgrade responses carry the rate-limit family.
		hdr := w.Header()
		hdr.Set("x-codex-primary-used-percent", "41.5")
		hdr.Set("x-codex-primary-window-minutes", "300")
		hdr.Set("x-codex-primary-reset-at", "1789106944")
		hdr.Set("x-codex-secondary-used-percent", "66")
		hdr.Set("x-codex-secondary-window-minutes", "10080")
		hdr.Set("x-codex-secondary-reset-at", "1789535344")
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"done"}`))
		<-r.Context().Done()
	}))
	defer upstreamSrv.Close()

	sel := &fakeSelector{
		decision: selected("w"),
		identity: Identity{WorkspaceID: "w", ChatGPTAccountID: "acct", AccessToken: "tok"},
	}
	front := httptest.NewServer(New(Options{
		UpstreamBase: upstreamSrv.URL + "/backend-api",
		Selector:     sel,
		Clock:        clock.System(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/backend-api/codex/responses"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"thread-id": []string{"ws-quota-thread"}},
	})
	if err != nil {
		t.Fatalf("dial through the proxy failed: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read through the bridge failed: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if sel.quotaWS != "w" {
		t.Fatalf("quota was not attributed to the serving workspace, got %q", sel.quotaWS)
	}
	if len(sel.quota) == 0 || sel.quota[0].Primary == nil {
		t.Fatal("no quota was ingested from the WebSocket upgrade response")
	}
	if got := sel.quota[0].Primary.UsedPercent; got != 41.5 {
		t.Fatalf("primary used_percent = %v, want 41.5", got)
	}
	if sel.quota[0].Secondary == nil || sel.quota[0].Secondary.Minutes != 10080 {
		t.Fatalf("secondary window not ingested: %+v", sel.quota[0].Secondary)
	}
}

// TestWebSocketIngestsInStreamQuotaEvents is the end-to-end form of the real-backend defect:
// no headers on the upgrade, quota delivered as a frame, and the proxy must record it while
// still forwarding the frame untouched.
func TestWebSocketIngestsInStreamQuotaEvents(t *testing.T) {
	const quotaFrame = `{"type":"codex.rate_limits","rate_limits":{"primary":` +
		`{"used_percent":57.5,"window_minutes":300,"reset_at":1789106944},` +
		`"secondary":{"used_percent":81,"window_minutes":10080,"reset_at":1789535344}}}`

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created"}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(quotaFrame))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed"}`))
		<-r.Context().Done()
	}))
	defer upstreamSrv.Close()

	sel := &fakeSelector{
		decision: selected("w"),
		identity: Identity{WorkspaceID: "w", ChatGPTAccountID: "acct", AccessToken: "tok"},
	}
	front := httptest.NewServer(New(Options{
		UpstreamBase: upstreamSrv.URL + "/backend-api",
		Selector:     sel,
		Clock:        clock.System(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/backend-api/codex/responses"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"thread-id": []string{"ws-event-thread"}},
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.CloseNow()

	// Every frame must still reach the client, byte for byte.
	var seen []string
	for i := 0; i < 3; i++ {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		seen = append(seen, string(data))
	}
	if seen[1] != quotaFrame {
		t.Fatal("the quota frame must be forwarded to the client unchanged, not swallowed")
	}

	// Give the post-forward observer a moment; it runs after the write by design.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sel.mu.Lock()
		got := len(sel.quota)
		sel.mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.quota) == 0 {
		t.Fatal("no quota was ingested from the in-stream event")
	}
	if sel.quotaWS != "w" {
		t.Fatalf("quota attributed to %q, want the serving workspace", sel.quotaWS)
	}
	if sel.quota[0].Primary == nil || sel.quota[0].Primary.UsedPercent != 57.5 {
		t.Fatalf("primary window wrong: %+v", sel.quota[0].Primary)
	}
	if sel.quota[0].Secondary == nil || sel.quota[0].Secondary.Minutes != 10080 {
		t.Fatalf("secondary window wrong: %+v", sel.quota[0].Secondary)
	}
}

// TestWebSocketRecordsFirstTokenLatency: every real turn is a WebSocket upgrade, so without
// this the product had no first-token measurement for any real traffic at all.
func TestWebSocketRecordsFirstTokenLatency(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		time.Sleep(40 * time.Millisecond) // model "thinking" before the first frame
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.created"}`))
		<-r.Context().Done()
	}))
	defer upstreamSrv.Close()

	sel := &fakeSelector{
		decision: selected("w"),
		identity: Identity{WorkspaceID: "w", ChatGPTAccountID: "a", AccessToken: "t"},
	}
	front := httptest.NewServer(New(Options{
		UpstreamBase: upstreamSrv.URL + "/backend-api",
		Selector:     sel,
		Clock:        clock.System(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/backend-api/codex/responses"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"thread-id": []string{"ws-timing-thread"}},
	})
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	// CloseNow drops the socket immediately, which is what a client going away looks like.
	// A polite Close handshake can leave both pumps waiting on each other in this fake.
	c.CloseNow()

	deadline := time.Now().Add(8 * time.Second)
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
		t.Fatal("the websocket turn was not recorded")
	}
	got := sel.records[0].FirstTokenMS
	if got < 0 {
		t.Fatal("first-token latency was not measured for a websocket turn")
	}
	if got < 20 {
		t.Fatalf("first token measured at %d ms, but the upstream waited 40 ms before its first frame", got)
	}
}
