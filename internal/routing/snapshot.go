// Package routing publishes immutable decision snapshots and evaluates admissions against
// them.
//
// The shape is the standard immutable-registry pattern: build a complete view, publish it
// with a single atomic pointer swap, and never touch the database on the decision path.
// Deliberately kept at the size a local account manager needs: no sharding, no cluster
// membership, no per-candidate ranking ring.
package routing

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

// Registry holds the current snapshot. Reads are lock-free so a streamed turn never waits
// on a dashboard write.
type Registry struct {
	cur     atomic.Pointer[policy.State]
	clk     clock.Clock
	mu      sync.Mutex
	version int64

	subsMu sync.Mutex
	subs   map[int]chan struct{}
	nextID int
}

func NewRegistry(clk clock.Clock) *Registry {
	r := &Registry{clk: clk, subs: map[int]chan struct{}{}}
	r.cur.Store(&policy.State{
		Version:    0,
		Workspaces: map[string]*policy.WorkspaceState{},
		StaleAfter: 10 * time.Minute,
	})
	return r
}

// Current returns the published snapshot. Callers must treat it as read-only.
func (r *Registry) Current() *policy.State { return r.cur.Load() }

// Publish swaps in a new snapshot and stamps it with a monotonically increasing version.
// A decision and the preview that justified it can be compared by this version.
func (r *Registry) Publish(next *policy.State) *policy.State {
	r.mu.Lock()
	r.version++
	next.Version = r.version
	r.mu.Unlock()
	r.cur.Store(next)
	r.notify()
	return next
}

// Evaluate runs the shared evaluator against the published snapshot.
func (r *Registry) Evaluate(req policy.Request) policy.Decision {
	return policy.Evaluate(r.Current(), req, r.clk.Now())
}

// EvaluateAgainst runs the evaluator against a caller-supplied state. Preview uses this so
// a hypothetical scenario cannot mutate live state, while still running the same code.
func (r *Registry) EvaluateAgainst(st *policy.State, req policy.Request, now time.Time) policy.Decision {
	return policy.Evaluate(st, req, now)
}

// Subscribe returns a channel that receives a token whenever a new snapshot is published.
// The channel is buffered and coalescing: a slow dashboard cannot block routing.
func (r *Registry) Subscribe() (<-chan struct{}, func()) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	id := r.nextID
	r.nextID++
	ch := make(chan struct{}, 1)
	r.subs[id] = ch
	return ch, func() {
		r.subsMu.Lock()
		defer r.subsMu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

func (r *Registry) notify() {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- struct{}{}:
		default: // already has a pending token; coalesce
		}
	}
}

// CloneForPreview deep-copies the published state so a scenario can be altered safely.
// Simulation must never reach live quota or ownership.
func CloneForPreview(src *policy.State) *policy.State {
	dst := &policy.State{
		Version:             src.Version,
		Order:               append([]string(nil), src.Order...),
		DefaultWorkspaceID:  src.DefaultWorkspaceID,
		Rules:               append([]policy.Rule(nil), src.Rules...),
		StaleAfter:          src.StaleAfter,
		HandoffBelowPercent: src.HandoffBelowPercent,
		Workspaces:          make(map[string]*policy.WorkspaceState, len(src.Workspaces)),
	}
	for id, ws := range src.Workspaces {
		cp := *ws
		cp.Windows = make(map[int64]policy.Window, len(ws.Windows))
		for k, v := range ws.Windows {
			if v.ResetsAt != nil {
				t := *v.ResetsAt
				v.ResetsAt = &t
			}
			cp.Windows[k] = v
		}
		if ws.EligibleModels != nil {
			cp.EligibleModels = make(map[string]bool, len(ws.EligibleModels))
			for k, v := range ws.EligibleModels {
				cp.EligibleModels[k] = v
			}
		}
		dst.Workspaces[id] = &cp
	}
	return dst
}
