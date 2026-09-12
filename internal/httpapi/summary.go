package httpapi

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// summaryWindow is how far back the Overview tiles look.
const summaryWindow = 7 * 24 * time.Hour

// buildSummary counts what actually happened. It never estimates a cost, never projects
// future usage, and reports -1 rather than 0 when a statistic has no data behind it.
func (a *API) buildSummary(ctx context.Context, now time.Time, workspaces, rules int) Summary {
	s := Summary{
		WindowHours:        int(summaryWindow.Hours()),
		Workspaces:         workspaces,
		RulesActive:        rules,
		MedianFirstTokenMS: -1,
	}
	since := now.Add(-summaryWindow)

	pricing := a.Svc.Pricing(ctx)
	s.Pricing = pricing

	rows, err := a.Svc.DB.SQL().QueryContext(ctx, `
		SELECT at, outcome, thread_id, first_token_ms, error_class, status_code,
		       input_tokens, cached_input_tokens, output_tokens, total_tokens
		FROM decisions WHERE at >= ? ORDER BY at`, since.Format(time.RFC3339Nano))
	if err != nil {
		return s
	}
	defer rows.Close()

	const buckets = 24
	turnsB := make([]float64, buckets)
	errB := make([]float64, buckets)
	latSum := make([]float64, buckets)
	latN := make([]float64, buckets)
	tokB := make([]float64, buckets)
	costB := make([]float64, buckets)

	threads := map[string]struct{}{}
	errCounts := map[string]int{}
	var firstTokens []int64

	for rows.Next() {
		var atRaw, outcome string
		var thread, errClass sql.NullString
		var first, status, inTok, cachedTok, outTok, totTok sql.NullInt64
		if rows.Scan(&atRaw, &outcome, &thread, &first, &errClass, &status,
			&inTok, &cachedTok, &outTok, &totTok) != nil {
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, atRaw)
		if perr != nil {
			continue
		}
		s.Turns++
		if outcome == "selected" {
			s.TurnsServed++
		} else {
			s.TurnsBlocked++
		}
		if thread.Valid && thread.String != "" {
			threads[thread.String] = struct{}{}
		}
		// An error class is only an error if it names one. A completed turn has none.
		failed := errClass.Valid && errClass.String != "" && errClass.String != "client_cancelled"
		if failed {
			errCounts[errClass.String]++
		}
		if first.Valid && first.Int64 >= 0 {
			firstTokens = append(firstTokens, first.Int64)
		}

		idx := int(at.Sub(since) / (summaryWindow / buckets))
		if idx < 0 {
			idx = 0
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		if totTok.Valid && totTok.Int64 > 0 {
			s.TurnsWithUsage++
			s.InputTokens += inTok.Int64
			s.CachedTokens += cachedTok.Int64
			s.OutputTokens += outTok.Int64
			s.TotalTokens += totTok.Int64
			c := pricing.EstimateCost(inTok.Int64, cachedTok.Int64, outTok.Int64)
			s.EstimatedCost += c
			tokB[idx] += float64(totTok.Int64)
			costB[idx] += c
		}

		turnsB[idx]++
		if failed {
			errB[idx]++
		}
		if first.Valid && first.Int64 >= 0 {
			latSum[idx] += float64(first.Int64)
			latN[idx]++
		}
	}

	s.Conversations = len(threads)
	if len(firstTokens) > 0 {
		sort.Slice(firstTokens, func(i, j int) bool { return firstTokens[i] < firstTokens[j] })
		s.MedianFirstTokenMS = firstTokens[len(firstTokens)/2]
	}
	totalErrors := 0
	for class, n := range errCounts {
		totalErrors += n
		if n > errCounts[s.TopErrorClass] {
			s.TopErrorClass = class
		}
	}
	if s.Turns > 0 {
		s.ErrorRatePercent = float64(totalErrors) / float64(s.Turns) * 100
	}

	lat := make([]float64, buckets)
	for i := range lat {
		if latN[i] > 0 {
			lat[i] = latSum[i] / latN[i]
		}
	}
	s.TurnsSeries, s.ErrorSeries, s.LatencySeries = turnsB, errB, lat
	s.TokenSeries, s.CostSeries = tokB, costB
	return s
}
