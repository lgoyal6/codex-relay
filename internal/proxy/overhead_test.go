package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

// streamingUpstream flushes its first SSE event immediately, then trickles.
func streamingUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Real upstreams send these on every generation response. Their presence is what
		// triggers quota ingestion in the proxy, so a benchmark upstream that omits them
		// measures a path no real turn takes.
		w.Header().Set("x-codex-primary-used-percent", "24.5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-at", "1789106944")
		w.Header().Set("x-codex-secondary-used-percent", "72.0")
		w.Header().Set("x-codex-secondary-window-minutes", "10080")
		w.Header().Set("x-codex-secondary-reset-at", "1789535344")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		fmt.Fprint(w, "event: a\ndata: first\n\n")
		f.Flush()
		for i := 0; i < 3; i++ {
			time.Sleep(50 * time.Millisecond)
			fmt.Fprintf(w, "event: b\ndata: chunk%d\n\n", i)
			f.Flush()
		}
	}))
}

func ttfb(t *testing.T, url string, body []byte, thread string) time.Duration {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("thread-id", thread)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	d := time.Since(start)
	_, _ = io.Copy(io.Discard, resp.Body)
	return d
}

// TestProxyFirstTokenOverhead measures what codex-relay adds to first-token latency with a Go
// upstream, isolating the proxy from the Python mock used by the shell benchmark.
func TestProxyFirstTokenOverhead(t *testing.T) {
	up := streamingUpstream()
	defer up.Close()

	sel := &fakeSelector{
		decision: selected("w"),
		identity: Identity{WorkspaceID: "w", ChatGPTAccountID: "acct", AccessToken: "tok"},
	}
	front := httptest.NewServer(New(Options{
		UpstreamBase: up.URL + "/backend-api",
		Selector:     sel,
		Clock:        clock.System(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer front.Close()

	body := bytes.Repeat([]byte("x"), 8000)
	payload := append([]byte(`{"model":"gpt-5.1-codex","pad":"`), append(body, []byte(`"}`)...)...)

	const n = 25
	for i := 0; i < 5; i++ { // warm
		ttfb(t, up.URL+"/backend-api/codex/responses", payload, "warm")
		ttfb(t, front.URL+"/backend-api/codex/responses", payload, fmt.Sprintf("warm%d", i))
	}

	var direct, proxied []time.Duration
	for i := 0; i < n; i++ {
		direct = append(direct, ttfb(t, up.URL+"/backend-api/codex/responses", payload, "d"))
		proxied = append(proxied, ttfb(t, front.URL+"/backend-api/codex/responses", payload, fmt.Sprintf("p%d", i)))
	}
	md, mp := median(direct), median(proxied)
	t.Logf("first-token direct   p50 = %v", md)
	t.Logf("first-token codexrelay p50 = %v", mp)
	t.Logf("ADDED                     = %v", mp-md)

	// The measured figure with an in-process upstream is a few hundred microseconds. The
	// threshold is deliberately far above that so this is not a flaky timing test, while
	// still catching the class of defect it was written for: a blocking call added to the
	// path between the upstream response and the client's first byte. A keychain read there
	// cost 18 ms per turn until it was cached; anything of that size fails here.
	if mp-md > 10*time.Millisecond {
		t.Errorf("codex-relay adds %v to first-token latency; something blocking was added to "+
			"the path between the upstream response and the first byte to the client", mp-md)
	}
}

func median(xs []time.Duration) time.Duration {
	s := append([]time.Duration(nil), xs...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[len(s)/2]
}

var _ = policy.ReasonDefault
