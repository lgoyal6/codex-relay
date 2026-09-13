package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// serveWebSocket bridges a client WebSocket to an upstream WebSocket.
//
// Codex 0.154.0 attempts a WebSocket upgrade on the responses route before falling back to
// HTTP SSE (observed live). Forwarding it avoids roughly a second of dead reconnect time per
// turn. Ownership and identity selection are identical to the HTTP path; only the transport
// differs.
func (p *Proxy) serveWebSocket(w http.ResponseWriter, r *http.Request, route Route) {
	if route.Class != ClassGeneration {
		p.refuseUnknown(w, r, route)
		return
	}
	started := p.opt.Clock.Now()
	threadID := conversationID(r)

	owner, _, _ := p.opt.Selector.OwnerOf(r.Context(), threadID)
	d := p.opt.Selector.Decide(r.Context(), policy.Request{OwnerWorkspaceID: owner, ThreadID: threadID})
	// Every exit from this path is recorded, exactly as on the HTTP path. A failure that
	// leaves no Activity row is a failure the user cannot explain later.
	// firstMS is -1 until a frame actually arrives, matching the HTTP path's convention that
	// "never measured" and "measured as zero" are different things.
	firstMS := int64(-1)
	// usage holds the model's own token counts, written on the pump goroutine when the turn
	// reports them and read here when the turn ends.
	var usage atomic.Pointer[upstream.TokenUsage]
	// Written on the dial path and on the pump goroutine, read when the turn ends, so these
	// follow the same atomic pattern as usage above rather than inviting a race.
	var upstreamStatus atomic.Int64
	var upgradeMS atomic.Int64
	upgradeMS.Store(-1)
	var streamErrMsg atomic.Pointer[string]
	record := func(status int, class string) {
		p.opt.Selector.RecordDecision(Record{
			At: started, ThreadID: threadID, Decision: d, Attempt: 1,
			StatusCode: status, ErrorClass: class,
			Usage:        usage.Load(),
			FirstTokenMS: firstMS,
			TotalMS:      p.opt.Clock.Now().Sub(started).Milliseconds(),
			// Most real turns take this path, so without transport recorded the history
			// could not tell which half of the proxy a failure came from.
			Transport:      "websocket",
			UpstreamStatus: int(upstreamStatus.Load()),
			UpstreamMS:     upgradeMS.Load(),
			ErrorMessage:   derefOr(streamErrMsg.Load()),
			FailurePhase:   wsPhaseFor(class),
		})
	}

	if d.Outcome != policy.OutcomeSelected {
		// Refuse the upgrade with a real HTTP status so the client can fall back or surface
		// the reason, rather than seeing an opaque dropped connection.
		record(http.StatusServiceUnavailable, "blocked_by_policy")
		writeProblem(w, http.StatusServiceUnavailable, d.Summary)
		return
	}
	id, err := p.opt.Selector.Identity(r.Context(), d.WorkspaceID)
	if err != nil {
		record(http.StatusBadGateway, "credential_unavailable")
		writeProblem(w, http.StatusBadGateway, fmt.Sprintf("could not use the selected workspace: %v", err))
		return
	}

	// Dial upstream first. If upstream refuses, the client sees a normal HTTP failure and
	// can fall back to SSE, instead of an accepted socket that immediately dies.
	target := strings.TrimSuffix(p.opt.UpstreamBase, "/") + upstreamPath(r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	target = toWebSocketURL(target)

	hdr := http.Header{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || strings.HasPrefix(lk, "sec-websocket-") || lk == "host" {
			continue
		}
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	hdr.Set("Authorization", "Bearer "+id.AccessToken)
	hdr.Set("ChatGPT-Account-ID", id.ChatGPTAccountID)

	dialCtx, cancelDial := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancelDial()
	dialStart := p.opt.Clock.Now()
	up, upResp, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPClient: p.opt.HTTPClient,
		HTTPHeader: hdr,
	})
	upgradeMS.Store(p.opt.Clock.Now().Sub(dialStart).Milliseconds())
	if upResp != nil {
		upstreamStatus.Store(int64(upResp.StatusCode))
	}
	if err != nil {
		status := http.StatusBadGateway
		if upResp != nil {
			status = upResp.StatusCode
		}
		p.opt.Logger.Warn("upstream websocket dial failed", "status", status, "error", err)
		record(status, "upstream_upgrade_refused")
		writeProblem(w, status, fmt.Sprintf("upstream websocket refused the connection: %v", err))
		return
	}
	// No read limit: a turn's frames are as large as the model makes them.
	up.SetReadLimit(-1)
	defer up.CloseNow()

	// Quota lives on the UPGRADE response, and this is the only chance to read it.
	//
	// This was missing until a real backend run exposed it. Against the mock upstream the
	// WebSocket upgrade always failed and Codex fell back to HTTP SSE, so the HTTP path's
	// ingestion covered every test. Against the real backend the upgrade succeeds, every
	// turn is a 101, and quota was never recorded at all: the dashboard showed no readings
	// after real turns, which is the one number this product exists to show.
	if upResp != nil {
		if snaps := upstream.ParseRateLimits(upResp.Header); len(snaps) > 0 {
			p.opt.Selector.ObserveQuota(r.Context(), id.WorkspaceID, snaps)
		}
	}

	down, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The client is Codex on loopback, not a browser; origin checking does not apply
		// here and the listener is bound to loopback.
		InsecureSkipVerify: true,
	})
	if err != nil {
		p.opt.Logger.Warn("client websocket accept failed", "error", err)
		record(http.StatusBadRequest, "client_upgrade_failed")
		return
	}
	down.SetReadLimit(-1)
	defer down.CloseNow()

	// Ownership is claimed here: after both sockets exist, and strictly before any frame is
	// forwarded, so no account-bound state can be exposed unbound.
	//
	// It is deliberately NOT claimed before the upstream dial. Codex tries this upgrade
	// first and falls back to SSE when it fails (observed live against 0.154.0 whenever the
	// backend does not speak WebSocket on this route). Claiming before the dial would bind a
	// conversation that never ran a turn, and that binding would then constrain the fallback
	// request. The dial itself carries no turn, so nothing is owed until frames flow.
	if threadID != "" {
		bind := p.opt.Selector.Claim
		if d.Primary == policy.ReasonHandoff {
			// The evaluator already decided this conversation moves; Claim would refuse it.
			bind = p.opt.Selector.Reassign
		}
		if err := bind(r.Context(), threadID, d.WorkspaceID); err != nil {
			record(http.StatusConflict, "ownership_conflict")
			_ = down.Close(websocket.StatusPolicyViolation,
				"this conversation is bound to another workspace")
			return
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Quota arrives IN-BAND on this transport, as codex.rate_limits events, because the
	// upgrade response carries no rate-limit headers. Only the upstream -> client direction
	// is inspected; the client never sends quota.
	// firstAtUnixNano records when the first frame of model output reached us. Every REAL
	// turn is a WebSocket upgrade, so without this Activity showed no first-token latency at
	// all for real traffic, only for the SSE fallback that real turns never take.
	//
	// It is atomic because it is written on the pump goroutine and read on this one after
	// the pumps end.
	var firstAtUnixNano atomic.Int64
	// streamClass records a failure the upstream delivered as a FRAME. A failed WebSocket
	// turn still upgraded with 101, so without this an account that is entirely out of quota
	// is recorded as a healthy turn and the dashboard reports no problem at all.
	var streamClass atomic.Pointer[string]
	observe := func(frame []byte) {
		firstAtUnixNano.CompareAndSwap(0, p.opt.Clock.Now().UnixNano())
		if snap, ok := upstream.MaybeRateLimitEvent(frame); ok {
			p.opt.Selector.ObserveQuota(context.WithoutCancel(ctx), id.WorkspaceID, []upstream.Snapshot{snap})
		}
		if u, ok := upstream.MaybeTokenUsage(frame); ok {
			usage.Store(&u)
		}
		if f, ok := upstream.MaybeStreamError(frame); ok {
			cls := f.Class
			streamClass.CompareAndSwap(nil, &cls)
			msg := f.Message
			streamErrMsg.CompareAndSwap(nil, &msg)
			p.opt.Logger.Warn("upstream refused a turn in-stream",
				"workspace", id.WorkspaceID, "class", f.Class, "status", f.Status)
		}
	}

	errc := make(chan error, 2)
	go func() { errc <- pump(ctx, down, up, nil) }()     // client -> upstream
	go func() { errc <- pump(ctx, up, down, observe) }() // upstream -> client

	// The first side to end tears down the other. A cancelled turn closes both directions
	// promptly rather than leaking a half-open socket.
	<-errc
	cancel()

	if ns := firstAtUnixNano.Load(); ns != 0 {
		firstMS = time.Unix(0, ns).Sub(started).Milliseconds()
	}

	class := ""
	if c := streamClass.Load(); c != nil {
		// An upstream refusal outranks a client disconnect: the turn failed for a reason
		// the user can act on, and that reason is what Activity should show.
		class = *c
	} else if r.Context().Err() != nil {
		class = "client_cancelled"
	}
	record(http.StatusSwitchingProtocols, class)
}

// pump copies frames one direction, preserving message type. Reading whole messages bounds
// memory per frame to what the peer actually sent.
// pump copies frames one direction, preserving message type. observe, when non-nil, is
// handed each frame AFTER it has been forwarded, so inspecting it can never delay delivery.
func pump(ctx context.Context, from, to *websocket.Conn, observe func([]byte)) error {
	for {
		typ, data, err := from.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				_ = to.Close(ce.Code, ce.Reason)
			}
			return err
		}
		if err := to.Write(ctx, typ, data); err != nil {
			return err
		}
		if observe != nil && typ == websocket.MessageText {
			observe(data)
		}
	}
}

func toWebSocketURL(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// wsPhaseFor maps a websocket failure class to how far the turn got. The upgrade either
// happened or it did not, and after it did, everything is in-stream.
func wsPhaseFor(class string) string {
	switch class {
	case "":
		return ""
	case "blocked_by_policy", "credential_unavailable", "ownership_conflict":
		return "admission"
	case "upstream_upgrade_refused":
		return "upstream_connect"
	case "client_upgrade_failed":
		return "client"
	case "client_cancelled":
		return "client"
	default:
		return "stream"
	}
}

// derefOr reads an optional string written from another goroutine.
func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
