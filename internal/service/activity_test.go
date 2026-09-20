package service

import (
	"context"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/upstream"
)

func TestActivityReturnsRecordedTokenCounts(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	want := upstream.TokenUsage{
		InputTokens:       120,
		CachedInputTokens: 80,
		OutputTokens:      30,
		TotalTokens:       150,
	}
	if err := svc.writeDecision(proxy.Record{
		At: time.Now().UTC(),
		Decision: policy.Decision{
			Outcome: policy.OutcomeSelected,
			Primary: policy.ReasonDefault,
			Summary: "served",
		},
		Attempt: 1,
		Usage:   &want,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := svc.Activity(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d activity rows, want 1", len(rows))
	}
	got := rows[0]
	if got.InputTokens == nil || *got.InputTokens != want.InputTokens ||
		got.CachedInputTokens == nil || *got.CachedInputTokens != want.CachedInputTokens ||
		got.OutputTokens == nil || *got.OutputTokens != want.OutputTokens ||
		got.TotalTokens == nil || *got.TotalTokens != want.TotalTokens {
		t.Fatalf("activity usage = input %v, cached %v, output %v, total %v; want %+v",
			got.InputTokens, got.CachedInputTokens, got.OutputTokens, got.TotalTokens, want)
	}
}
