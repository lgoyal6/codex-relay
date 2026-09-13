package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

// A quota refusal is retried on another workspace, and the client never sees the 429.
//
// This is the failure the usage poller cannot prevent: a workspace can read as healthy and
// still refuse, because the reading is up to one poll interval old. The retry is only safe
// before any byte reaches the client, which is the window this exercises.
func TestQuotaRefusalIsRetriedOnAnotherWorkspace(t *testing.T) {
	var mu sync.Mutex
	var sawAuth []string

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(sawAuth)
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail":"usage limit reached"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
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
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, ChatGPTAccountID: "acct-" + ws, AccessToken: "tok-" + ws}, nil
	}

	p := newProxy(t, sel, up.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-5.1-codex"}`))
	r.Header.Set("thread-id", "t-1")
	r.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("client saw %d, want 200: the refusal should never reach it", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sawAuth) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (refusal then retry)", len(sawAuth))
	}
	if sawAuth[0] != "Bearer tok-ws-a" {
		t.Errorf("first attempt used %q, want ws-a", sawAuth[0])
	}
	// The retry must carry the OTHER workspace's credential, not a re-send on the same one.
	if sawAuth[1] != "Bearer tok-ws-b" {
		t.Errorf("retry used %q, want ws-b", sawAuth[1])
	}
	if len(sel.reassigns) != 1 || !strings.HasSuffix(sel.reassigns[0], "->ws-b") {
		t.Errorf("ownership was not moved to the retry workspace: %v", sel.reassigns)
	}
}

// With nowhere to retry, the real refusal is surfaced. Inventing a different error here would
// hide what upstream actually said.
func TestQuotaRefusalIsSurfacedWhenThereIsNoAlternative(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"usage limit reached"}`))
	}))
	defer up.Close()

	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(req policy.Request) policy.Decision {
		if len(req.Exclude) > 0 {
			return policy.Decision{Outcome: policy.OutcomeBlocked, Primary: policy.ReasonNoEligible}
		}
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, AccessToken: "tok-" + ws}, nil
	}

	p := newProxy(t, sel, up.URL)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("thread-id", "t-2")
	p.ServeHTTP(rec, r)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client saw %d, want the real 429 passed through", rec.Code)
	}
	if calls != 1 {
		t.Errorf("upstream called %d times, want 1: there was nowhere to retry", calls)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "usage limit") {
		t.Errorf("upstream's own message was lost: %s", body)
	}
	_ = context.Background()
}
