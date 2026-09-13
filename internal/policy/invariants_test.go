package policy

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// randomState builds an arbitrary but valid state: any number of workspaces, any mix of
// paused, broken credentials, window shapes and rules.
func randomState(rnd *rand.Rand) (*State, Request) {
	n := 1 + rnd.Intn(5)
	st := &State{
		Version:    rnd.Int63n(1000),
		Workspaces: map[string]*WorkspaceState{},
		StaleAfter: time.Duration(rnd.Intn(20)) * time.Minute,
	}
	if rnd.Intn(3) == 0 {
		st.HandoffBelowPercent = float64(rnd.Intn(10))
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ws%d", i)
		ws := &WorkspaceState{
			ID: id, Name: "W" + id, AccountID: fmt.Sprintf("acct%d", rnd.Intn(3)),
			Paused:       rnd.Intn(4) == 0,
			CredentialOK: rnd.Intn(5) != 0,
			Windows:      map[int64]Window{},
		}
		for _, m := range []int64{300, 10080} {
			if rnd.Intn(4) == 0 {
				continue // a plan that does not report this window
			}
			w := Window{Minutes: m, UsedPercent: rnd.Float64() * 100}
			if rnd.Intn(5) != 0 {
				t := base.Add(time.Duration(rnd.Intn(400)-100) * time.Hour)
				w.ResetsAt = &t
			}
			w.ObservedAt = base.Add(-time.Duration(rnd.Intn(60)) * time.Minute)
			ws.Windows[m] = w
		}
		if rnd.Intn(3) == 0 {
			ws.EligibleModels = map[string]bool{"gpt-5.1-codex": true}
		}
		st.Workspaces[id] = ws
		st.Order = append(st.Order, id)
	}
	st.DefaultWorkspaceID = st.Order[rnd.Intn(len(st.Order))]

	for i := 0; i < rnd.Intn(3); i++ {
		src := st.Order[rnd.Intn(len(st.Order))]
		pref := st.Order[rnd.Intn(len(st.Order))]
		kind := KindReserve
		if rnd.Intn(2) == 0 {
			kind = KindPrefer
		}
		st.Rules = append(st.Rules, Rule{
			ID: fmt.Sprintf("r%d", i), Kind: kind, Enabled: rnd.Intn(5) != 0,
			SourceWorkspaceID: src, PreferredWorkspaceID: pref,
			Window:           WindowRef{Minutes: []int64{300, 10080}[rnd.Intn(2)]},
			Comparison:       []Comparison{AtOrBelow, Below}[rnd.Intn(2)],
			RemainingPercent: rnd.Float64() * 100,
			ResetComparison:  []ResetComparison{AtLeast, AtMost}[rnd.Intn(2)],
			ResetHours:       float64(rnd.Intn(200)),
		})
	}

	req := Request{}
	if rnd.Intn(2) == 0 {
		req.Model = "gpt-5.1-codex"
	}
	if rnd.Intn(2) == 0 {
		req.OwnerWorkspaceID = st.Order[rnd.Intn(len(st.Order))]
	}
	req.ThreadID = "t"
	return st, req
}

// The safety properties that must hold for EVERY state, not just the ones I thought to write
// by hand. A violation here is a real routing defect: spending on a paused or protected
// workspace, or naming a workspace while claiming nothing was spent.
func TestEvaluateInvariantsUnderRandomStates(t *testing.T) {
	for seed := int64(0); seed < 3000; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		st, req := randomState(rnd)
		d := Evaluate(st, req, base)

		switch d.Outcome {
		case OutcomeSelected:
			ws, ok := st.Workspaces[d.WorkspaceID]
			if !ok {
				t.Fatalf("seed %d: selected %q which does not exist", seed, d.WorkspaceID)
			}
			if ws.Paused {
				t.Fatalf("seed %d: selected paused workspace %q", seed, d.WorkspaceID)
			}
			if !ws.CredentialOK {
				t.Fatalf("seed %d: selected %q with an unusable credential", seed, d.WorkspaceID)
			}
		case OutcomeBlocked:
			if d.WorkspaceID != "" {
				t.Fatalf("seed %d: blocked but still named workspace %q", seed, d.WorkspaceID)
			}
		default:
			t.Fatalf("seed %d: unknown outcome %q", seed, d.Outcome)
		}
		if d.Primary == "" {
			t.Fatalf("seed %d: decision carries no reason code", seed)
		}
		if d.Summary == "" {
			t.Fatalf("seed %d: decision carries no human summary", seed)
		}
	}
}

// The evaluator must be a pure function of (state, request, now). Go randomises map
// iteration, so anything that ranges over a map without sorting can pick a different
// workspace on different runs of identical input. That would make routing unreproducible and
// make a dashboard preview disagree with the live decision it is supposed to predict.
func TestEvaluateIsDeterministic(t *testing.T) {
	for seed := int64(0); seed < 2000; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		st, req := randomState(rnd)

		first := Evaluate(st, req, base)
		for i := 0; i < 8; i++ {
			again := Evaluate(st, req, base)
			if first.WorkspaceID != again.WorkspaceID || first.Outcome != again.Outcome || first.Primary != again.Primary {
				t.Fatalf("seed %d: not deterministic: %s/%s/%s then %s/%s/%s",
					seed, first.Outcome, first.WorkspaceID, first.Primary,
					again.Outcome, again.WorkspaceID, again.Primary)
			}
		}
	}
}

// Evaluate must not mutate the state it is given. The live snapshot is shared by every
// concurrent turn and is documented as read-only, so a write here is a data race that would
// only show up under load.
func TestEvaluateDoesNotMutateState(t *testing.T) {
	for seed := int64(0); seed < 1000; seed++ {
		rnd := rand.New(rand.NewSource(seed))
		st, req := randomState(rnd)

		before, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		Evaluate(st, req, base)
		after, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatalf("seed %d: Evaluate mutated the shared state", seed)
		}
	}
}
