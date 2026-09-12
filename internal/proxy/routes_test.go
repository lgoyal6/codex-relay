package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObservedRoutesAreClassified(t *testing.T) {
	cases := map[string]Class{
		"/backend-api/codex/responses":                     ClassGeneration,
		"/backend-api/codex/models?client_version=0.154.0": ClassModelCatalog,
		"/backend-api/wham/usage":                          ClassIdentityScoped,
		"/backend-api/wham/accounts/check":                 ClassIdentityScoped,
		"/backend-api/wham/settings/user":                  ClassClientOwned,
		"/backend-api/ps/mcp":                              ClassClientOwned,
		"/backend-api/ps/plugins/installed":                ClassClientOwned,
		"/backend-api/plugins/featured":                    ClassClientOwned,
		"/backend-api/codex/analytics-events/events":       ClassClientOwned,
	}
	for path, want := range cases {
		if got := Classify(path).Class; got != want {
			t.Errorf("Classify(%q) = %s, want %s", path, got, want)
		}
	}
}

func TestUnknownRoutesFailClosed(t *testing.T) {
	for _, p := range []string{
		"/backend-api/codex/some-future-endpoint",
		"/backend-api/wham/billing/charge",
		"/backend-api/anything",
	} {
		if got := Classify(p).Class; got != ClassUnknown {
			t.Errorf("Classify(%q) = %s, want unknown so it fails closed", p, got)
		}
	}
}

// TestModelCatalogIsRecordedAndPassedThrough covers the only place a workspace states which
// models it can serve. Without this the evaluator's model-eligibility rule could never fire
// on real data, and the body must still reach the client byte for byte.
func TestModelCatalogIsRecordedAndPassedThrough(t *testing.T) {
	const catalog = `{"models":[{"id":"gpt-5.1-codex","slug":"gpt-5.1-codex","supported_reasoning_levels":["low","high"]},{"id":"gpt-5.1-codex-mini","slug":"gpt-5.1-codex-mini"}]}`
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, catalog)
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "TOKEN_WORK"},
	}
	p := newProxy(t, sel, back.URL)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/backend-api/codex/models?client_version=0.154.0", nil))

	if rec.Body.String() != catalog {
		t.Fatalf("the catalog body must reach the client unchanged, got %q", rec.Body.String())
	}
	if sel.modelsWS != "work" {
		t.Fatalf("eligibility must be attributed to the identity that answered, got %q", sel.modelsWS)
	}
	want := []string{"gpt-5.1-codex", "gpt-5.1-codex-mini"}
	if len(sel.models) != len(want) {
		t.Fatalf("recorded models = %v, want %v", sel.models, want)
	}
	for i := range want {
		if sel.models[i] != want[i] {
			t.Fatalf("recorded models = %v, want %v", sel.models, want)
		}
	}
}

// TestUnparsableCatalogRecordsNothing keeps "unknown" distinct from "supports nothing". A
// catalog we cannot read must leave eligibility unobserved, not empty.
func TestUnparsableCatalogRecordsNothing(t *testing.T) {
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `<!doctype html><p>a gateway error page</p>`)
	}))
	defer back.Close()

	sel := &fakeSelector{
		decision: selected("work"),
		identity: Identity{WorkspaceID: "work", ChatGPTAccountID: "acct_work", AccessToken: "T"},
	}
	p := newProxy(t, sel, back.URL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/backend-api/codex/models", nil))

	if sel.modelsWS != "" || len(sel.models) != 0 {
		t.Fatalf("an unreadable catalog must record nothing, got ws=%q models=%v", sel.modelsWS, sel.models)
	}
	if !strings.Contains(rec.Body.String(), "gateway error") {
		t.Fatalf("the body must still reach the client, got %q", rec.Body.String())
	}
}

// TestFileUploadsStayWithTheSignedInClient pins the first-release limitation in code.
//
// The reason is structural, not a preference: Codex's file-upload request carries only auth
// headers (codex-api/src/files.rs authorized_request), so it has no thread-id and we cannot
// tell which workspace will serve the turn that references the file. Pooling it would mean
// guessing, and a wrong guess uploads under one account and references from another.
//
// If this ever changes upstream - if a thread-id appears on /files - this test is the place
// that should fail, so the decision gets revisited deliberately.
func TestFileUploadsStayWithTheSignedInClient(t *testing.T) {
	for _, p := range []string{
		"/backend-api/files",
		"/backend-api/files/file_abc123/uploaded",
	} {
		got := Classify(p)
		if got.Class != ClassClientOwned {
			t.Errorf("Classify(%q) = %s, want client_owned: the upload has no conversation "+
				"identity, so it must keep the client's own credential rather than being "+
				"pooled or failing closed", p, got.Class)
		}
	}
}

// TestFileRoutesDoNotSwallowGeneration guards the new patterns against over-matching.
func TestFileRoutesDoNotSwallowGeneration(t *testing.T) {
	if got := Classify("/backend-api/codex/responses").Class; got != ClassGeneration {
		t.Fatalf("generation must still classify as generation, got %s", got)
	}
	if got := Classify("/backend-api/codex/models").Class; got != ClassModelCatalog {
		t.Fatalf("model catalog must still classify correctly, got %s", got)
	}
	// A plausible future account-bound mutation must still fail closed.
	if got := Classify("/backend-api/files/file_abc/delete").Class; got != ClassUnknown {
		t.Fatalf("an unrecognised file operation must fail closed, got %s", got)
	}
}
