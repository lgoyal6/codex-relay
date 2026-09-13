package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

// maxBufferedBody bounds how much of a request we hold in memory to read its identity
// headers and model. Codex request bodies are large (64 KB was typical in the live capture)
// but bounded; anything beyond this is streamed straight through without inspection.
const maxBufferedBody = 8 << 20 // 8 MiB

// serveGeneration admits one model turn.
//
// Order matters and is load-bearing:
//  1. read conversation identity from headers (thread-id), never from the body
//  2. consult existing ownership
//  3. evaluate policy with that ownership
//  4. persist ownership BEFORE any account-bound state reaches the client
//  5. forward with the selected identity, streaming without buffering the response
func (p *Proxy) serveGeneration(w http.ResponseWriter, r *http.Request) {
	started := p.opt.Clock.Now()
	threadID := conversationID(r)
	model, rawBody := modelFromBody(r)

	owner, _, err := p.opt.Selector.OwnerOf(r.Context(), threadID)
	if err != nil {
		p.opt.Logger.Warn("ownership lookup failed", "error", err)
	}

	d := p.opt.Selector.Decide(r.Context(), policy.Request{
		Model:            model,
		OwnerWorkspaceID: owner,
		ThreadID:         threadID,
	})

	if d.Outcome != policy.OutcomeSelected {
		// Nothing is spent and the client is told why, in its own error channel.
		p.opt.Selector.RecordDecision(Record{
			At: started, ThreadID: threadID, Model: model, Decision: d,
			Attempt: 1, StatusCode: http.StatusServiceUnavailable, ErrorClass: "blocked_by_policy",
			FailurePhase: "admission", Transport: "http", ErrorMessage: d.Summary,
			TotalMS: p.opt.Clock.Now().Sub(started).Milliseconds(),
		})
		writeProblem(w, http.StatusServiceUnavailable, d.Summary)
		return
	}

	// Persist ownership before exposing account-bound state. If this fails we do not
	// proceed: an unrecorded binding is how conversations end up split across accounts.
	if threadID != "" {
		bind := p.opt.Selector.Claim
		if d.Primary == policy.ReasonHandoff {
			// The evaluator already decided this conversation moves; Claim would refuse it.
			bind = p.opt.Selector.Reassign
		}
		if err := bind(r.Context(), threadID, d.WorkspaceID); err != nil {
			p.opt.Selector.RecordDecision(Record{
				At: started, ThreadID: threadID, Model: model, Decision: d,
				Attempt: 1, StatusCode: http.StatusConflict, ErrorClass: "ownership_conflict",
				FailurePhase: "admission", Transport: "http", ErrorMessage: err.Error(),
				TotalMS: p.opt.Clock.Now().Sub(started).Milliseconds(),
			})
			writeProblem(w, http.StatusConflict, fmt.Sprintf(
				"This conversation is already bound to a different workspace. %v. Start a new conversation to use a different workspace.", err))
			return
		}
	}

	id, err := p.opt.Selector.Identity(r.Context(), d.WorkspaceID)
	if err != nil {
		p.opt.Selector.RecordDecision(Record{
			At: started, ThreadID: threadID, Model: model, Decision: d,
			Attempt: 1, StatusCode: http.StatusBadGateway, ErrorClass: "credential_unavailable",
			FailurePhase: "admission", Transport: "http", ErrorMessage: err.Error(),
			TotalMS: p.opt.Clock.Now().Sub(started).Milliseconds(),
		})
		writeProblem(w, http.StatusBadGateway, fmt.Sprintf("could not use the selected workspace: %v", err))
		return
	}

	req, err := p.buildUpstream(r, &id)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, err.Error())
		return
	}

	// The request context is the client's. When Codex cancels a turn, the context is
	// cancelled, the upstream request is torn down, and we stop. We never retry a turn
	// after output has begun: replay safety has not been demonstrated for this protocol,
	// so a mid-stream failure is surfaced rather than silently re-sent.
	upstreamStart := p.opt.Clock.Now()
	resp, err := p.opt.HTTPClient.Do(req)
	upstreamMS := p.opt.Clock.Now().Sub(upstreamStart).Milliseconds()
	if err != nil {
		cls := "transport"
		if r.Context().Err() != nil {
			cls = "client_cancelled"
		}
		p.opt.Selector.RecordDecision(Record{
			At: started, ThreadID: threadID, Model: model, Decision: d,
			Attempt: 1, ErrorClass: cls, FailurePhase: "upstream_connect", Transport: "http",
			ErrorMessage: err.Error(),
			TotalMS:      p.opt.Clock.Now().Sub(started).Milliseconds(),
		})
		writeUpstreamError(w, err)
		return
	}
	defer func() { resp.Body.Close() }()

	// A quota refusal is the one failure worth retrying, and this is the only place it is
	// safe: the upstream answered, nothing has been written to the client yet, and the body
	// is still in memory. Once a single byte has gone downstream a retry would duplicate
	// output, which is why nothing below this point retries.
	//
	// The poller can be up to its interval out of date, so a workspace can read as healthy
	// and still refuse. Upstream's answer wins over the stored reading.
	attempt := 1
	if resp.StatusCode == http.StatusTooManyRequests && rawBody != nil && threadID != "" {
		retryDecision := p.opt.Selector.Decide(r.Context(), policy.Request{
			Model:            model,
			OwnerWorkspaceID: owner,
			ThreadID:         threadID,
			Exclude:          []string{d.WorkspaceID},
		})
		if retryDecision.Outcome == policy.OutcomeSelected && retryDecision.WorkspaceID != d.WorkspaceID {
			if retried, ok := p.retryElsewhere(w, r, retryDecision, rawBody, started, threadID, model, resp); ok {
				resp = retried
				d = retryDecision
				attempt = 2
				defer func() { resp.Body.Close() }()
			}
		}
	}

	if snaps := upstream.ParseRateLimits(resp.Header); len(snaps) > 0 {
		p.opt.Selector.ObserveQuota(r.Context(), id.WorkspaceID, snaps)
	}

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Codex-Pool-Workspace", id.WorkspaceID)
	w.Header().Set("X-Codex-Pool-Reason", string(d.Primary))
	w.WriteHeader(resp.StatusCode)

	// The SSE path carries quota in-band too, for the same reason the WebSocket path does:
	// the real backend does not put rate-limit headers on a streamed generation response.
	var inStreamClass string
	// The upstream's own words. MaybeStreamError already parsed this and it was being
	// dropped; a user debugging a failed turn saw the bucket and never the reason.
	var streamErrMessage string
	var usage *upstream.TokenUsage
	onFrame := func(frame []byte) {
		if u, ok := upstream.MaybeTokenUsage(frame); ok {
			usage = &u
		}
		if snap, ok := upstream.MaybeRateLimitEvent(frame); ok {
			p.opt.Selector.ObserveQuota(context.WithoutCancel(r.Context()), id.WorkspaceID,
				[]upstream.Snapshot{snap})
		}
		if f, ok := upstream.MaybeStreamError(frame); ok && inStreamClass == "" {
			inStreamClass = f.Class
			streamErrMessage = f.Message
		}
	}

	firstToken, total, streamClass := p.stream(r, w, resp.Body, started, onFrame)

	// How a stream ENDED is its own failure class. A turn the user cancelled and a turn
	// that completed both leave a 200 behind, and telling them apart afterwards is the
	// whole point of classifying cancellation separately from a transient failure.
	class := streamClass
	if class == "" {
		class = inStreamClass
	}
	if class == "" {
		class = errorClassFor(resp.StatusCode)
	}

	p.opt.Selector.RecordDecision(Record{
		At: started, ThreadID: threadID, Model: model, Decision: d,
		Attempt: attempt, StatusCode: resp.StatusCode,
		FirstTokenMS: firstToken, TotalMS: total, Usage: usage,
		ErrorClass:     class,
		Transport:      "http",
		UpstreamStatus: resp.StatusCode,
		UpstreamMS:     upstreamMS,
		ErrorMessage:   streamErrMessage,
		FailurePhase:   phaseFor(class, resp.StatusCode),
	})
}

// stream copies the upstream body to the client, flushing as it goes so streamed output is
// not held back. It reports first-token and total timings and how the stream ended.
//
// Nothing on this path writes to the database or waits on the dashboard.
func (p *Proxy) stream(r *http.Request, w http.ResponseWriter, body io.Reader, started time.Time, onFrame func([]byte)) (firstTokenMS, totalMS int64, endedBy string) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	// line accumulates the current SSE line so an event split across two reads is still
	// seen whole. It is bounded: a line longer than this cannot be a quota event, and the
	// cap applies only to the SCAN, never to the bytes forwarded to the client.
	const maxLine = 256 << 10
	var line []byte
	var first time.Time

	for {
		n, err := body.Read(buf)
		if n > 0 {
			if first.IsZero() {
				first = p.opt.Clock.Now()
			}
			chunk := buf[:n]
			// Forward FIRST. Inspection must never delay the client's bytes.
			if _, werr := w.Write(chunk); werr != nil {
				endedBy = "client_cancelled"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if onFrame != nil {
				for _, b := range chunk {
					if b == '\n' {
						if len(line) > 0 {
							onFrame(line)
							line = line[:0]
						}
						continue
					}
					if len(line) < maxLine {
						line = append(line, b)
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				if r.Context().Err() != nil {
					endedBy = "client_cancelled"
				} else {
					endedBy = "upstream_stream_interrupted"
				}
			}
			break
		}
	}
	if onFrame != nil && len(line) > 0 {
		onFrame(line)
	}
	// Deliberately NOT: "if the request context is now cancelled, call it cancelled".
	// A client that reads the whole stream and then closes promptly cancels the request
	// context as a matter of course, so that check labelled ordinary completed turns as
	// cancelled in Activity. Cancellation is only inferred from an actual mid-stream read
	// or write failure, above.

	now := p.opt.Clock.Now()
	// -1 means "no byte ever arrived", which is different from a first token that genuinely
	// landed in under a millisecond. Collapsing both to 0 made a real measurement look like
	// a missing one, and Activity then had no timed row at all on a fast local upstream.
	firstTokenMS = -1
	if !first.IsZero() {
		firstTokenMS = first.Sub(started).Milliseconds()
	}
	return firstTokenMS, now.Sub(started).Milliseconds(), endedBy
}

// conversationID reads the stable conversation identity.
//
// Captured live from codex-cli 0.154.0: the client sends `thread-id` and `session-id`
// headers plus an `x-codex-turn-metadata` JSON blob. thread-id is the conversation; session
// and turn ids have narrower scopes and must not be used as the ownership key.
func conversationID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("thread-id")); v != "" {
		return v
	}
	if raw := r.Header.Get("x-codex-turn-metadata"); raw != "" {
		var meta struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal([]byte(raw), &meta); err == nil && meta.ThreadID != "" {
			return meta.ThreadID
		}
	}
	// session-id is a last resort; it is a wider scope than a thread but still per-client.
	return strings.TrimSpace(r.Header.Get("session-id"))
}

// modelFromBody reads the requested model so eligibility can be checked, and returns the
// buffered bytes. The body is restored afterwards so forwarding is unaffected.
//
// The bytes are returned because a quota refusal is retried on another workspace, and a retry
// needs the same body again. Buffering already happened for the model read, so replay costs
// nothing extra.
func modelFromBody(r *http.Request) (string, []byte) {
	if r.Body == nil || r.ContentLength > maxBufferedBody {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBufferedBody))
	if err != nil {
		return "", nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))

	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", raw
	}
	return body.Model, raw
}

func errorClassFor(status int) string {
	switch {
	case status == 401 || status == 403:
		return "credential"
	case status == 429:
		return "quota"
	case status >= 500:
		return "upstream"
	case status >= 400:
		return "request"
	}
	return ""
}

// retryElsewhere re-sends an unsent turn to a different workspace after a quota refusal.
//
// It returns the new response and true only when the retry actually produced one. On any
// failure the caller keeps the original refusal, so the user sees upstream's real answer
// rather than an error invented here.
func (p *Proxy) retryElsewhere(
	w http.ResponseWriter, r *http.Request, d policy.Decision, rawBody []byte,
	started time.Time, threadID, model string, first *http.Response,
) (*http.Response, bool) {
	// Record the refusal that caused the move, so the history shows both halves.
	p.opt.Selector.RecordDecision(Record{
		At: started, ThreadID: threadID, Model: model, Decision: d,
		Attempt: 1, StatusCode: first.StatusCode, ErrorClass: "quota",
		FailurePhase: "upstream_status", Transport: "http", UpstreamStatus: first.StatusCode,
		ErrorMessage: "upstream refused this turn on quota; retried on another workspace",
		TotalMS:      p.opt.Clock.Now().Sub(started).Milliseconds(),
	})

	if err := p.opt.Selector.Reassign(r.Context(), threadID, d.WorkspaceID); err != nil {
		p.opt.Logger.Warn("could not reassign after a quota refusal", "error", err)
		return nil, false
	}
	id, err := p.opt.Selector.Identity(r.Context(), d.WorkspaceID)
	if err != nil {
		p.opt.Logger.Warn("no identity for the retry workspace", "error", err)
		return nil, false
	}

	r.Body = io.NopCloser(bytes.NewReader(rawBody))
	r.ContentLength = int64(len(rawBody))
	req, err := p.buildUpstream(r, &id)
	if err != nil {
		return nil, false
	}
	resp, err := p.opt.HTTPClient.Do(req)
	if err != nil {
		p.opt.Logger.Warn("retry after quota refusal failed", "error", err)
		return nil, false
	}
	first.Body.Close()
	return resp, true
}

// phaseFor says how far a turn got, which the status code alone cannot express. A 200 that
// failed mid-stream and a 429 refused before any byte left are different problems with
// different fixes, and a user reading history needs to see which one they had.
func phaseFor(class string, status int) string {
	switch {
	case class == "":
		return ""
	case class == "client_cancelled":
		return "client"
	case status >= 400:
		return "upstream_status"
	default:
		return "stream"
	}
}
