package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

func TestImageRequestsPreservePayloadAndUseSelectedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, path     string
		large, chunked bool
	}{
		{name: "generation", path: "/backend-api/codex/images/generations"},
		{name: "edit", path: "/backend-api/codex/images/edits"},
		{name: "large edit", path: "/backend-api/codex/images/edits", large: true},
		{name: "chunked large edit", path: "/backend-api/codex/images/edits", large: true, chunked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"gpt-image-2","prompt":"a room"}`
			if tc.large {
				input = `{"images":[{"image_url":"data:image/png;base64,` + strings.Repeat("A", maxBufferedBody+100) + `"}],"model":"gpt-image-2","prompt":"a room"}`
			}
			output := `{"created":1,"data":[{"b64_json":"` + strings.Repeat("B", 300000) + `"}]}`
			calls := 0
			back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, []byte(input)) {
					t.Errorf("image input changed: read %d of %d bytes, error %v", len(body), len(input), err)
				}
				if r.URL.Path != tc.path {
					t.Errorf("path = %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer TOKEN_WORK" || r.Header.Get("ChatGPT-Account-ID") != "acct_work" {
					t.Error("selected credentials not used")
				}
				if r.Header.Get("x-codex-image-turn-id") != "image-turn" || r.Header.Get("originator") != "chatgpt_cca" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("image request headers changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-codex-imagegen-request-id", "image-result")
				fmt.Fprint(w, output)
			}))
			defer back.Close()
			sel := &fakeSelector{identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "TOKEN_WORK"}, decideFn: func(req policy.Request) policy.Decision {
				// The chat model catalog does not establish image model eligibility.
				if req.Model != "" || req.ThreadID != "" || req.IsSubagent {
					t.Errorf("wrong image admission metadata: %+v", req)
				}
				return selected("work")
			}}
			req := httptest.NewRequest("POST", tc.path, strings.NewReader(input))
			if tc.chunked {
				req.ContentLength = -1
			}
			req.Header.Set("Authorization", "Bearer CLIENT_TOKEN")
			req.Header.Set("ChatGPT-Account-ID", "acct_client")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("originator", "chatgpt_cca")
			req.Header.Set("x-codex-image-turn-id", "image-turn")
			rec := httptest.NewRecorder()
			newProxy(t, sel, back.URL).ServeHTTP(rec, req)
			if rec.Code != 200 || rec.Body.String() != output {
				t.Fatalf("image response changed: status %d, size %d", rec.Code, rec.Body.Len())
			}
			if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("x-codex-imagegen-request-id") != "image-result" || rec.Header().Get("X-Codex-Pool-Workspace") != "work" {
				t.Error("image response headers changed")
			}
			if calls != 1 || len(sel.claims) != 0 {
				t.Errorf("calls %d, invented conversation bindings %v", calls, sel.claims)
			}
			if len(sel.records) != 1 || sel.records[0].RequestKind != "image" || sel.records[0].ErrorClass != "" {
				t.Errorf("image activity not recorded correctly: %+v", sel.records)
			}
		})
	}
}

func TestImageAdmissionRefusalsDoNotReachUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sel    *fakeSelector
		status int
	}{
		{"unauthenticated", &fakeSelector{authFn: func(*http.Request) (string, error) { return "", fmt.Errorf("locked") }}, 401},
		{"policy blocked", &fakeSelector{decision: policy.Decision{Outcome: policy.OutcomeBlocked, Summary: "paused"}}, 503},
		{"credential missing", &fakeSelector{decision: selected("work"), idErr: fmt.Errorf("missing")}, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("refused request reached upstream") }))
			defer back.Close()
			rec := httptest.NewRecorder()
			newProxy(t, tc.sel, back.URL).ServeHTTP(rec, httptest.NewRequest("POST", "/backend-api/codex/images/generations", strings.NewReader(`{"model":"gpt-image-2"}`)))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

func TestImageErrorsArePassedThroughWithoutRetry(t *testing.T) {
	for _, status := range []int{400, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			output := `{"error":{"message":"image request refused"}}`
			back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprint(w, output)
			}))
			defer back.Close()
			sel := &fakeSelector{decision: selected("work"), identity: Identity{WorkspaceID: "work"}}
			req := httptest.NewRequest("POST", "/backend-api/codex/images/generations", strings.NewReader(`{"model":"gpt-image-2"}`))
			req.Header.Set("thread-id", "future-client-thread")
			rec := httptest.NewRecorder()
			newProxy(t, sel, back.URL).ServeHTTP(rec, req)
			if rec.Code != status || rec.Body.String() != output || calls != 1 {
				t.Fatalf("error changed or retried: status %d, calls %d", rec.Code, calls)
			}
		})
	}
}

func TestImageRoutesRemainNarrow(t *testing.T) {
	for _, path := range []string{"/backend-api/codex/images/delete", "/backend-api/codex/images/generations/other", "/arbitrary/codex/images/edits"} {
		if Classify(path).Class != ClassUnknown {
			t.Errorf("unexpectedly authorized %s", path)
		}
	}
	sel := &fakeSelector{decideFn: func(policy.Request) policy.Decision {
		t.Error("non-POST image request entered policy")
		return selected("work")
	}}
	rec := httptest.NewRecorder()
	newProxy(t, sel, "http://unused.invalid").ServeHTTP(rec, httptest.NewRequest("GET", "/backend-api/codex/images/generations", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}
