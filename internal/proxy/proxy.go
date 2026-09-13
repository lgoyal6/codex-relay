// Package proxy forwards Codex traffic to the ChatGPT backend under a selected pooled
// identity, preserving streaming behaviour and conversation ownership.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// Identity is a resolved, ready-to-use credential for one workspace.
type Identity struct {
	WorkspaceID string
	// ChatGPTAccountID is sent as the ChatGPT-Account-ID header. Codex sends its own value
	// from its own auth.json; we always replace it so the workspace we selected is the
	// workspace that is billed.
	ChatGPTAccountID string
	AccessToken      string
}

// Selector resolves a decision into a usable identity, refreshing credentials if needed.
type Selector interface {
	// Decide runs the shared evaluator for an admission.
	Decide(ctx context.Context, req policy.Request) policy.Decision
	// Identity returns credentials for a chosen workspace. Refresh is serialized per
	// credential chain by the implementation.
	Identity(ctx context.Context, workspaceID string) (Identity, error)
	// OwnerOf returns the workspace already bound to a conversation.
	OwnerOf(ctx context.Context, threadID string) (string, bool, error)
	// Claim binds a conversation to a workspace before account-bound state is exposed.
	Claim(ctx context.Context, threadID, workspaceID string) error
	// Reassign re-points a conversation after the evaluator decided to hand it off.
	Reassign(ctx context.Context, threadID, workspaceID string) error
	// Authenticate resolves the caller to an API key id, or refuses when the relay is
	// locked. An empty id with no error means an unlocked relay and an anonymous caller.
	Authenticate(ctx context.Context, r *http.Request) (keyID string, err error)
	// ObserveQuota records rate-limit evidence seen on a real response.
	ObserveQuota(ctx context.Context, workspaceID string, snaps []upstream.Snapshot)
	// ObserveModels records the model catalog one identity reported. An empty list is
	// treated as "not observed", never as "supports nothing".
	ObserveModels(ctx context.Context, workspaceID string, slugs []string)
	// RecordDecision appends a bounded audit row. It must never block the turn.
	RecordDecision(rec Record)
}

// Record is one activity row. It deliberately carries no request body, no prompt text and
// no credential material.
type Record struct {
	At         time.Time
	ThreadID   string
	Model      string
	Decision   policy.Decision
	Attempt    int
	StatusCode int
	// FirstTokenMS is -1 when no byte was ever received, and >= 0 when measured. A measured
	// zero is a real reading on a fast local upstream, not a missing one.
	FirstTokenMS int64
	TotalMS      int64
	ErrorClass   string
	// Usage is the model's own reported token counts, when the turn reported any.
	Usage *upstream.TokenUsage
	// APIKeyID attributes the turn to a relay key, empty when the caller was anonymous.
	APIKeyID string

	// ErrorMessage is what upstream actually said. ErrorClass buckets a failure so rules can
	// act on it; this is the sentence a person needs to know WHY, and losing it means a user
	// debugging a broken turn sees "quota" and nothing else.
	ErrorMessage string
	// FailurePhase says how far the turn got: "admission", "upstream_connect", "upstream_status",
	// "stream". A failure before any byte left is a different problem from one mid-stream, and
	// the status code alone cannot tell them apart.
	FailurePhase string
	// UpstreamStatus is what the backend returned, which is not always what the client saw.
	// A quota refusal that is retried elsewhere shows 429 here and 200 to the client.
	UpstreamStatus int
	// Transport is "http" or "websocket". The two paths have different failure modes and
	// different latency, and until now the history could not distinguish them.
	Transport string
	// UpstreamMS is how long the upstream took to answer at all: on HTTP the response
	// headers, on WebSocket the upgrade. It is NOT a latency budget you can subtract
	// FirstTokenMS from, because after the upgrade the model still has to start
	// generating. It answers "was upstream slow to accept this", nothing more.
	UpstreamMS int64
}

// Options configures the proxy.
type Options struct {
	// UpstreamBase is the real ChatGPT backend root, e.g. https://chatgpt.com/backend-api.
	UpstreamBase string
	Selector     Selector
	Clock        clock.Clock
	Logger       *slog.Logger
	// UpstreamProxy resolves the outbound proxy for a request, or nil for a direct
	// connection. It is consulted per request rather than baked into the transport, so
	// changing it in the dashboard takes effect without a restart and without dropping the
	// connections a streaming turn is using.
	UpstreamProxy func(*http.Request) (*url.URL, error)

	// HTTPClient is used for non-streaming and streaming forwards alike. Its timeout must
	// be zero: a long turn is not a hung request.
	HTTPClient *http.Client
}

// Proxy is the loopback listener Codex is pointed at.
type Proxy struct {
	opt Options
}

func New(opt Options) *Proxy {
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{
			// No client timeout: a streamed turn can legitimately run for many minutes.
			// Idle and connection timeouts live in the transport instead.
			Transport: &http.Transport{
				Proxy: func(r *http.Request) (*url.URL, error) {
					if opt.UpstreamProxy != nil {
						if u, err := opt.UpstreamProxy(r); err != nil || u != nil {
							return u, err
						}
					}
					// Falling back to the environment keeps a machine that already works
					// behind a corporate proxy working with no configuration at all.
					return http.ProxyFromEnvironment(r)
				},
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				ExpectContinueTimeout: time.Second,
				MaxIdleConns:          32,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		}
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Proxy{opt: opt}
}

// hopByHop headers are never forwarded.
var hopByHop = map[string]bool{
	"connection": true, "proxy-connection": true, "keep-alive": true,
	"transfer-encoding": true, "te": true, "trailer": true, "upgrade": true,
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := Classify(r.URL.Path)

	if isWebSocketUpgrade(r) {
		p.serveWebSocket(w, r, route)
		return
	}

	switch route.Class {
	case ClassGeneration:
		p.serveGeneration(w, r)
	case ClassModelCatalog, ClassIdentityScoped:
		p.serveIdentityScoped(w, r, route)
	case ClassClientOwned:
		p.forwardClientOwned(w, r)
	default:
		p.refuseUnknown(w, r, route)
	}
}

// refuseUnknown fails closed. The message names the route so a future Codex version's new
// endpoint shows up as an actionable diagnostic instead of silently getting someone's token.
func (p *Proxy) refuseUnknown(w http.ResponseWriter, r *http.Request, route Route) {
	kind := "request"
	if IsMutation(r.Method) {
		kind = "change"
	}
	p.opt.Logger.Warn("refused unclassified route",
		"method", r.Method, "path", r.URL.Path, "class", route.Class.String())
	writeProblem(w, http.StatusBadGateway, fmt.Sprintf(
		"codex-relay does not recognise this %s (%s %s), so it did not attach any pooled account credential to it. "+
			"This usually means Codex is newer than this version of codex-relay. Check Diagnostics in the dashboard.",
		kind, r.Method, r.URL.Path))
}

// forwardClientOwned passes the client's own credential through untouched.
func (p *Proxy) forwardClientOwned(w http.ResponseWriter, r *http.Request) {
	req, err := p.buildUpstream(r, nil)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, err.Error())
		return
	}
	resp, err := p.opt.HTTPClient.Do(req)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// serveIdentityScoped answers reads that describe exactly one identity.
func (p *Proxy) serveIdentityScoped(w http.ResponseWriter, r *http.Request, route Route) {
	d := p.opt.Selector.Decide(r.Context(), policy.Request{Model: modelHint(r)})
	if d.Outcome != policy.OutcomeSelected {
		writeProblem(w, http.StatusServiceUnavailable, d.Summary)
		return
	}
	id, err := p.opt.Selector.Identity(r.Context(), d.WorkspaceID)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, fmt.Sprintf("could not use the selected workspace: %v", err))
		return
	}
	req, err := p.buildUpstream(r, &id)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, err.Error())
		return
	}
	resp, err := p.opt.HTTPClient.Do(req)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if snaps := upstream.ParseRateLimits(resp.Header); len(snaps) > 0 {
		p.opt.Selector.ObserveQuota(r.Context(), id.WorkspaceID, snaps)
	}

	// The catalog is the only place a workspace states which models it can serve. Read it
	// here, where we already know which identity answered, then pass the same bytes on.
	body := io.Reader(resp.Body)
	if route.Class == ClassModelCatalog {
		var slugs []string
		body, slugs = captureCatalog(resp)
		if len(slugs) > 0 {
			p.opt.Selector.ObserveModels(r.Context(), id.WorkspaceID, slugs)
		}
	}

	copyHeaders(w.Header(), resp.Header)
	// Name the identity that answered, so a reader never mistakes a single-identity answer
	// for a pooled total.
	w.Header().Set("X-Codex-Pool-Workspace", id.WorkspaceID)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, body)
}

// buildUpstream rewrites the request for the real backend. When id is non-nil, the client's
// Authorization and ChatGPT-Account-ID are REPLACED, never merged.
func (p *Proxy) buildUpstream(r *http.Request, id *Identity) (*http.Request, error) {
	target := strings.TrimSuffix(p.opt.UpstreamBase, "/") + upstreamPath(r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Del("Host")
	if id != nil {
		req.Header.Set("Authorization", "Bearer "+id.AccessToken)
		req.Header.Set("ChatGPT-Account-ID", id.ChatGPTAccountID)
	}
	req.ContentLength = r.ContentLength
	return req, nil
}

// upstreamPath strips our local mount point so /backend-api/... maps onto the real backend
// root without doubling the prefix.
func upstreamPath(p string) string {
	if i := strings.Index(p, "/backend-api"); i >= 0 {
		return p[i+len("/backend-api"):]
	}
	return p
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeProblem(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"codex_pool_error","source":"codex-relay"}}`, msg)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		// The client went away. Nothing to write.
		return
	}
	writeProblem(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func modelHint(r *http.Request) string { return r.URL.Query().Get("model") }
