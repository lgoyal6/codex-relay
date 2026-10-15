package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

func TestClientCancellationCancelsUpstreamWithoutRetry(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.created\ndata: {}\n\n"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer up.Close()

	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(policy.Request) policy.Decision {
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, ChatGPTAccountID: "acct-" + ws, AccessToken: "token"}, nil
	}
	p := newProxy(t, sel, up.URL)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	r.Header.Set("thread-id", "cancel-thread")
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach upstream")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not finish after cancellation")
	}
	if calls != 1 {
		t.Fatalf("upstream received %d attempts after cancellation, want exactly one", calls)
	}
}
