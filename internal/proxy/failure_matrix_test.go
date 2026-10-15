package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

type failureRoundTripFunc func(*http.Request) (*http.Response, error)

func (f failureRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type errorAfterChunk struct {
	chunk string
	done  bool
}

func (r *errorAfterChunk) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.chunk), nil
	}
	return 0, errors.New("upstream connection reset after stream prefix")
}

func (*errorAfterChunk) Close() error { return nil }

type chunksReader struct {
	chunks []string
	index  int
}

func (r *chunksReader) Read(p []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

func (*chunksReader) Close() error { return nil }

type failSecondWrite struct {
	header http.Header
	status int
	writes int
	body   bytes.Buffer
}

func (w *failSecondWrite) Header() http.Header  { return w.header }
func (w *failSecondWrite) WriteHeader(code int) { w.status = code }
func (w *failSecondWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		return 0, errors.New("downstream disconnected")
	}
	return w.body.Write(p)
}
func (*failSecondWrite) Flush() {}

func TestHTTPFailuresBeforePrefixDoNotLoop(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls, alternatives atomic.Int32
			sel := &fakeSelector{owner: "ws-a"}
			sel.decideFn = func(req policy.Request) policy.Decision {
				if len(req.Exclude) > 0 {
					alternatives.Add(1)
					return policy.Decision{Outcome: policy.OutcomeBlocked, Primary: policy.ReasonNoEligible}
				}
				return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
			}
			sel.identityFn = func(ws string) (Identity, error) { return Identity{WorkspaceID: ws, AccessToken: "token"}, nil }
			p := newProxy(t, sel, "http://upstream.test")
			p.opt.HTTPClient = &http.Client{Transport: failureRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("refused")), Request: r}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("thread-id", "pre-prefix-failure")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != status || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d, want original status %d and one request", rec.Code, calls.Load(), status)
			}
			if status != http.StatusTooManyRequests && alternatives.Load() != 0 {
				t.Fatalf("non-quota status consulted alternate workspaces %d times", alternatives.Load())
			}
		})
	}
}

func TestStreamedFailuresAfterPrefixAreNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name, frame, class string
	}{
		{"quota", `{"type":"error","status":429,"error":{"message":"limit reached"}}`, "quota"},
		{"upstream", `{"type":"error","status":503,"error":{"message":"unavailable"}}`, "upstream"},
		{"request", `{"type":"error","status":400,"error":{"message":"invalid request"}}`, "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			sel := &fakeSelector{}
			sel.decideFn = func(req policy.Request) policy.Decision {
				if len(req.Exclude) > 0 {
					return selected("ws-b")
				}
				return selected("ws-a")
			}
			sel.identityFn = func(ws string) (Identity, error) {
				return Identity{WorkspaceID: ws, AccessToken: "token-" + ws}, nil
			}
			p := newProxy(t, sel, "http://upstream.test")
			p.opt.HTTPClient = &http.Client{Transport: failureRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
					"event: error\ndata: " + tc.frame + "\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("thread-id", "stream-failure")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status=%d attempts=%d, want the partial HTTP 200 and one upstream attempt", rec.Code, calls.Load())
			}
			sel.mu.Lock()
			defer sel.mu.Unlock()
			if len(sel.records) != 1 || sel.records[0].ErrorClass != tc.class {
				t.Fatalf("stream failure record = %+v, want one %q failure", sel.records, tc.class)
			}
		})
	}
}

func TestInterruptedStreamKeepsLateUsageOnServingAttempt(t *testing.T) {
	var calls atomic.Int32
	sel := &fakeSelector{decision: selected("ws-a"), identity: Identity{WorkspaceID: "ws-a", AccessToken: "token"}}
	p := newProxy(t, sel, "http://upstream.test")
	p.opt.HTTPClient = &http.Client{Transport: failureRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := &errorAfterChunk{chunk: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n"}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: r}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "interrupted-with-usage")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if calls.Load() != 1 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("attempts=%d body=%q; interrupted response must remain a single streamed attempt", calls.Load(), rec.Body.String())
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.records) != 1 {
		t.Fatalf("recorded %d attempts, want one", len(sel.records))
	}
	record := sel.records[0]
	if record.Decision.WorkspaceID != "ws-a" || record.ErrorClass != "upstream_stream_interrupted" {
		t.Fatalf("interrupted attempt attribution = %+v", record)
	}
	if record.Usage == nil || record.Usage.InputTokens != 9 || record.Usage.OutputTokens != 4 || record.Usage.TotalTokens != 13 {
		t.Fatalf("late usage was not retained on the serving attempt: %+v", record.Usage)
	}
}

func TestDownstreamDisconnectBeforeLateUsageDoesNotRecordUnseenUsage(t *testing.T) {
	var calls atomic.Int32
	sel := &fakeSelector{decision: selected("ws-a"), identity: Identity{WorkspaceID: "ws-a", AccessToken: "token"}}
	p := newProxy(t, sel, "http://upstream.test")
	p.opt.HTTPClient = &http.Client{Transport: failureRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := &chunksReader{chunks: []string{
			"event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n",
		}}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: r}, nil
	})}
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("thread-id", "disconnected-before-usage")
	w := &failSecondWrite{header: make(http.Header)}
	p.ServeHTTP(w, req)

	if calls.Load() != 1 || w.status != http.StatusOK || !strings.Contains(w.body.String(), "response.created") {
		t.Fatalf("status=%d calls=%d body=%q", w.status, calls.Load(), w.body.String())
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	if len(sel.records) != 1 || sel.records[0].ErrorClass != "client_cancelled" || sel.records[0].Usage != nil {
		t.Fatalf("disconnected activity recorded unseen usage or wrong failure: %+v", sel.records)
	}
}

func TestCancellationDuringRetrySelectionDoesNotStartAnotherAttempt(t *testing.T) {
	var calls atomic.Int32
	pickAlternative := make(chan struct{})
	continueSelection := make(chan struct{})
	sel := &fakeSelector{owner: "ws-a"}
	sel.decideFn = func(req policy.Request) policy.Decision {
		if len(req.Exclude) > 0 {
			close(pickAlternative)
			<-continueSelection
			return policy.Decision{Outcome: policy.OutcomeBlocked, Primary: policy.ReasonNoEligible}
		}
		return policy.Decision{Outcome: policy.OutcomeSelected, WorkspaceID: "ws-a", Primary: policy.ReasonOwnerBound}
	}
	sel.identityFn = func(ws string) (Identity, error) {
		return Identity{WorkspaceID: ws, AccessToken: "token"}, nil
	}
	p := newProxy(t, sel, "http://upstream.test")
	p.opt.HTTPClient = &http.Client{Transport: failureRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"usage limit"}`)), Request: r}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	req.Header.Set("thread-id", "cancel-during-retry-selection")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-pickAlternative:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("the quota refusal did not reach alternate-workspace selection")
	}
	cancel()
	close(continueSelection)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not finish after cancellation")
	}
	if calls.Load() != 1 || rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d attempts=%d, want original refusal and one attempt", rec.Code, calls.Load())
	}
	sel.mu.Lock()
	defer sel.mu.Unlock()
	for _, reassignment := range sel.reassigns {
		if strings.HasSuffix(reassignment, "->ws-b") {
			t.Fatalf("cancelled retry changed conversation ownership: %v", sel.reassigns)
		}
	}
}
