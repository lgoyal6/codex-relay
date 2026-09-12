package httpapi

import (
	"fmt"
	"sort"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

// buildProjections extrapolates the rate ALREADY OBSERVED inside each quota window.
//
// The arithmetic is deliberately simple and stated on screen: a window that is X% used, Y
// hours into its own period, has been consumed at X/Y percent per hour. Continue that line and
// you get a time to empty.
//
// Its weakness is exactly as visible as its result. Early in a window a single burst looks
// like a permanent rate, so confidence is reported alongside the number and is never "high".
// This is a straight line through observed usage, not a model of how anyone works.
func buildProjections(st *policy.State, now time.Time) []Projection {
	var out []Projection
	for _, id := range st.Order {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		for minutes, w := range ws.Windows {
			if w.ResetsAt == nil || minutes <= 0 {
				continue
			}
			windowLen := time.Duration(minutes) * time.Minute
			hoursToReset := w.ResetsAt.Sub(now).Hours()
			if hoursToReset < 0 {
				hoursToReset = 0
			}
			// How far into this window we are: the period ends at ResetsAt.
			elapsed := windowLen.Hours() - hoursToReset
			used := 100 - w.RemainingPercent()

			p := Projection{
				WorkspaceID:      ws.ID,
				WorkspaceName:    ws.Name,
				WindowMinutes:    minutes,
				WindowLabel:      policy.HumanWindow(minutes),
				RemainingPercent: w.RemainingPercent(),
				HoursToReset:     hoursToReset,
				HoursToEmpty:     -1,
				Confidence:       "none",
			}

			switch {
			case elapsed <= 0.05:
				p.Basis = "This window has only just started, so there is no rate to extrapolate yet."
			case used <= 0:
				p.Basis = "Nothing has been used in this window, so nothing is on course to run out."
			default:
				p.BurnPercentPerHour = used / elapsed
				if p.BurnPercentPerHour > 0 {
					p.HoursToEmpty = w.RemainingPercent() / p.BurnPercentPerHour
					p.WillRunOutBeforeReset = p.HoursToEmpty < hoursToReset
				}
				// Confidence is about how much of the window the rate is measured over. A
				// rate from six minutes of a five-hour window is not worth acting on.
				frac := elapsed / windowLen.Hours()
				switch {
				case frac < 0.15:
					p.Confidence = "low"
				default:
					p.Confidence = "moderate"
				}
				p.Basis = fmt.Sprintf(
					"%.0f%% used over %s of this %s window, continued in a straight line.",
					used, humanHours(elapsed), p.WindowLabel)
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkspaceID != out[j].WorkspaceID {
			return out[i].WorkspaceID < out[j].WorkspaceID
		}
		return out[i].WindowMinutes < out[j].WindowMinutes
	})
	return out
}

// buildPoolTotals combines workspaces per window duration.
//
// A combined figure across unlike plans is not a capacity anyone can spend, so it is expressed
// as ACCOUNT EQUIVALENTS ("2.4 accounts' worth left") rather than as a credit balance, and it
// carries the plan mix so an average over a Plus and a Pro account is visibly that.
func buildPoolTotals(st *policy.State, now time.Time, plans map[string]string) []PoolTotal {
	type acc struct {
		sum   float64
		n     int
		plans map[string]struct{}
		label string
	}
	byWindow := map[int64]*acc{}
	for _, id := range st.Order {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		for minutes, w := range ws.Windows {
			a := byWindow[minutes]
			if a == nil {
				a = &acc{plans: map[string]struct{}{}, label: policy.HumanWindow(minutes)}
				byWindow[minutes] = a
			}
			a.sum += w.RemainingPercent()
			a.n++
			if p := plans[ws.AccountID]; p != "" {
				a.plans[p] = struct{}{}
			}
		}
	}

	var out []PoolTotal
	for minutes, a := range byWindow {
		if a.n == 0 {
			continue
		}
		var planList []string
		for p := range a.plans {
			planList = append(planList, p)
		}
		sort.Strings(planList)
		out = append(out, PoolTotal{
			WindowMinutes:      minutes,
			WindowLabel:        a.label,
			Workspaces:         a.n,
			AverageRemaining:   a.sum / float64(a.n),
			AccountEquivalents: a.sum / 100,
			Plans:              planList,
			MixedPlans:         len(planList) > 1,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WindowMinutes < out[j].WindowMinutes })
	return out
}

func humanHours(h float64) string {
	if h >= 24 {
		return fmt.Sprintf("%.1f days", h/24)
	}
	if h < 1 {
		return fmt.Sprintf("%.0f minutes", h*60)
	}
	return fmt.Sprintf("%.1f hours", h)
}
