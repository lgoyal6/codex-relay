package service

// Routing, as the pool drains.
//
// The dashboard can already answer "which workspace serves the next turn, and why". It could
// not answer the question a person actually plans around, which is "when does that change".
// A reserve threshold and a handoff floor are both invisible until the day they fire, and a
// rule that reads correctly can still surprise you at 24% remaining.
//
// This walks one workspace's quota down and asks the real evaluator at every step, so what
// comes back is not a second model of the routing rules. It is the routing rules. Anything
// else would drift from the engine the first time either changed, and a simulation that
// disagrees with the thing it simulates is worse than none.

import (
	"context"
	"fmt"
	"math"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/routing"
)

// SimulationBand is one contiguous run of remaining-percent values that route the same way.
//
// Bands rather than points: a hundred rows saying the same thing is not an answer, and the
// only interesting values are the ones where the answer changes.
type SimulationBand struct {
	FromPercent   float64           `json:"from_percent"`
	ToPercent     float64           `json:"to_percent"`
	Outcome       policy.Outcome    `json:"outcome"`
	WorkspaceID   string            `json:"workspace_id,omitempty"`
	WorkspaceName string            `json:"workspace_name,omitempty"`
	Primary       policy.ReasonCode `json:"primary_reason"`
	Summary       string            `json:"summary"`
	// DrainedDetail is what became of the workspace being drained, in its own words. At a
	// handover the summary only names the winner, and the question a person has at that
	// boundary is what happened to the account they were watching.
	DrainedDetail string `json:"drained_detail,omitempty"`

	// drainedReason ends a band. It is not serialised: the dashboard shows the sentence,
	// not the code.
	drainedReason policy.ReasonCode
}

// Simulation is the whole walk for one workspace's window.
type Simulation struct {
	WorkspaceID    string           `json:"workspace_id"`
	WorkspaceName  string           `json:"workspace_name"`
	WindowMinutes  int64            `json:"window_minutes"`
	WindowLabel    string           `json:"window_label"`
	FromPercent    float64          `json:"from_percent"`
	Bands          []SimulationBand `json:"bands"`
	HandoffFloor   float64          `json:"handoff_floor_percent"`
	OtherUnchanged bool             `json:"other_workspaces_unchanged"`
}

// SimulateDrain walks workspaceID's window from its current remaining percent down to zero.
//
// Only that one workspace moves. Draining everything at once would be a different and much
// vaguer question, and the honest caption for this one is that the rest of the pool is held
// where it is, which the result says so the dashboard can print it.
func (s *Service) SimulateDrain(ctx context.Context, workspaceID string, windowMinutes int64, model string) (Simulation, error) {
	st := routing.CloneForPreview(s.Registry.Current())
	ws := st.Workspaces[workspaceID]
	if ws == nil {
		return Simulation{}, fmt.Errorf("unknown workspace %q", workspaceID)
	}

	// Default to the window that is actually binding. Simulating a weekly window while a
	// five-hour window is the thing about to run out would answer the wrong question.
	if windowMinutes == 0 {
		lowest := math.MaxFloat64
		for m, w := range ws.Windows {
			if rem := 100 - w.UsedPercent; rem < lowest {
				lowest, windowMinutes = rem, m
			}
		}
	}
	w, ok := ws.Windows[windowMinutes]
	if !ok {
		return Simulation{}, fmt.Errorf("%s has no %s window to simulate", ws.Name, policy.HumanWindow(windowMinutes))
	}

	start := math.Round(100 - w.UsedPercent)
	if start < 0 {
		start = 0
	}
	out := Simulation{
		WorkspaceID: workspaceID, WorkspaceName: ws.Name,
		WindowMinutes: windowMinutes, WindowLabel: policy.HumanWindow(windowMinutes),
		FromPercent: start, HandoffFloor: st.HandoffBelowPercent, OtherUnchanged: len(st.Workspaces) > 1,
	}

	for p := start; p >= 0; p-- {
		pct := p
		d, err := s.Preview(ctx, nil, model, []Scenario{{
			WorkspaceID: workspaceID, WindowMinutes: windowMinutes, RemainingPercent: &pct,
		}})
		if err != nil {
			return Simulation{}, err
		}
		name := ""
		if target := st.Workspaces[d.WorkspaceID]; target != nil {
			name = target.Name
		}
		// The reason code decides where a band ends; the detail is only for display. They
		// are not interchangeable: the detail embeds the percentage being simulated, so
		// comparing it would end the band on every single step and report a hundred bands
		// that all say the same thing.
		drained, drainedReason := "", policy.ReasonCode("")
		for _, c := range d.Candidates {
			if c.WorkspaceID == workspaceID && !c.Eligible {
				drained, drainedReason = c.Detail, c.Reason
				break
			}
		}
		// Extend the open band while the answer holds; the reason matters as much as the
		// workspace, because "still this one, for a different reason" is a real change.
		if n := len(out.Bands); n > 0 &&
			out.Bands[n-1].WorkspaceID == d.WorkspaceID &&
			out.Bands[n-1].Outcome == d.Outcome &&
			out.Bands[n-1].Primary == d.Primary &&
			out.Bands[n-1].drainedReason == drainedReason {
			out.Bands[n-1].ToPercent = pct
			continue
		}
		out.Bands = append(out.Bands, SimulationBand{
			FromPercent: pct, ToPercent: pct, Outcome: d.Outcome,
			WorkspaceID: d.WorkspaceID, WorkspaceName: name,
			Primary: d.Primary, Summary: d.Summary,
			DrainedDetail: drained, drainedReason: drainedReason,
		})
	}
	return out, nil
}
