package pricing

import (
	"math"
	"testing"

	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

func TestNormalizeStripsSnapshotDateAndContextMarker(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":                "claude-opus-5",
		"claude-sonnet-4-5-20250929":   "claude-sonnet-4-5",
		"Claude-Fable-5-1[1m]":         "claude-fable-5-1",
		" claude-opus-4-8 ":            "claude-opus-4-8",
		"claude-3-5-haiku-20241022":    "claude-3-5-haiku",
		"<synthetic>":                  "<synthetic>",
		"claude-opus-4-5-20251101[1m]": "claude-opus-4-5",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The two rows below are Claude Code's own lastCost figures for two sessions,
// observed in a .claude.json on 2026-09-03 with every cache write at the
// 1-hour TTL. Reproducing them is what makes the table's opus-5 row and the
// cost formula evidence rather than belief.
func TestCostReproducesClaudeCodesOwnLastCost(t *testing.T) {
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		u    usage.Tokens
		want float64
	}{
		{usage.Tokens{Input: 1156, Output: 22964, CacheWrite: 140538, CacheWrite1h: 140538, CacheRead: 3996349, TTLKnown: true}, 3.9834345},
		{usage.Tokens{Input: 2500, Output: 302621, CacheWrite: 1165499, CacheWrite1h: 1165499, CacheRead: 148827918, TTLKnown: true}, 93.646974},
	}
	for _, c := range cases {
		got := table.Cost("claude-opus-5", c.u)
		if !got.Priced || !got.Verified {
			t.Fatalf("opus-5 must be priced and verified, got %+v", got)
		}
		if math.Abs(got.USD-c.want) > 1e-6 {
			t.Errorf("Cost(%+v) = %.7f, want %.7f", c.u, got.USD, c.want)
		}
		if got.AssumedTTLTokens != 0 {
			t.Errorf("no TTL should be assumed when the breakdown is known, got %d", got.AssumedTTLTokens)
		}
	}
}

func TestCostAssumesFiveMinuteTTLWhenTheTranscriptHasNoBreakdown(t *testing.T) {
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := table.Cost("claude-opus-5", usage.Tokens{CacheWrite: 1_000_000})
	if math.Abs(got.USD-6.25) > 1e-9 {
		t.Errorf("1M cache-write tokens at the assumed 5m rate should cost 6.25, got %.4f", got.USD)
	}
	if got.AssumedTTLTokens != 1_000_000 {
		t.Errorf("AssumedTTLTokens = %d, want 1000000", got.AssumedTTLTokens)
	}
}

func TestUnknownModelIsUnpricedNotGuessed(t *testing.T) {
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := table.Cost("<synthetic>", usage.Tokens{Input: 10, Output: 10})
	if got.Priced || got.USD != 0 {
		t.Errorf("unknown model must be unpriced with zero cost, got %+v", got)
	}
}

func TestEveryEmbeddedRowHasCacheMultipliersConsistentWithItsInputPrice(t *testing.T) {
	table, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range table.ModelIDs() {
		p := table.Models[id]
		if p.Input <= 0 || p.Output <= 0 {
			t.Errorf("%s: input/output must be positive", id)
		}
		if math.Abs(p.CacheWrite5m-1.25*p.Input) > 1e-9 || math.Abs(p.CacheWrite1h-2*p.Input) > 1e-9 {
			t.Errorf("%s: cache-write multipliers are not 1.25x/2x input: %+v", id, p)
		}
		if p.CacheRead > p.Input {
			t.Errorf("%s: cache read must not cost more than input", id)
		}
	}
}
