// Package pricing prices token usage from an embedded, versioned list-price
// table. The table is data, not code: its provenance and date travel with it,
// and a model that is not in it is reported as unpriced rather than guessed.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

//go:embed prices.json
var pricesJSON []byte

// Price is the list price of one model in USD per million tokens.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	// Verified is true when the row was copied from a documented price table
	// rather than recalled from memory.
	Verified bool   `json:"verified"`
	Note     string `json:"note,omitempty"`
}

// Table is the price list with its provenance.
type Table struct {
	Source string           `json:"source"`
	AsOf   string           `json:"as_of"`
	Models map[string]Price `json:"models"`
}

// Load returns the embedded table.
func Load() (*Table, error) {
	var t Table
	if err := json.Unmarshal(pricesJSON, &t); err != nil {
		return nil, fmt.Errorf("pricing: embedded prices.json: %w", err)
	}
	return &t, nil
}

// ModelIDs lists the priced models in a stable order.
func (t *Table) ModelIDs() []string {
	ids := make([]string, 0, len(t.Models))
	for id := range t.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// Normalize maps a transcript model id onto a price-table key: lower case,
// the "[1m]" context marker and a trailing -YYYYMMDD snapshot date removed,
// so claude-sonnet-4-5-20250929 prices as claude-sonnet-4-5.
func Normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "[1m]")
	return dateSuffix.ReplaceAllString(m, "")
}

// Cost is the priced value of one request.
type Cost struct {
	USD float64
	// Priced is false when the model is not in the table; USD is then zero.
	Priced bool
	// Verified is false when the price used came from memory rather than a
	// documented table.
	Verified bool
	// AssumedTTLTokens counts cache-write tokens priced at the 5-minute rate
	// because the transcript did not record which TTL wrote them.
	AssumedTTLTokens int64
}

// Cost prices one request. Cache writes are priced per TTL when the transcript
// carries the breakdown; otherwise every cache-write token is priced at the
// cheaper 5-minute rate and counted in AssumedTTLTokens, so the report can say
// how much of the total rests on that assumption.
func (t *Table) Cost(model string, u usage.Tokens) Cost {
	p, ok := t.Models[Normalize(model)]
	if !ok {
		return Cost{}
	}
	const million = 1_000_000
	usd := float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*p.CacheRead
	c := Cost{Priced: true, Verified: p.Verified}
	if u.TTLKnown {
		usd += float64(u.CacheWrite5m)*p.CacheWrite5m + float64(u.CacheWrite1h)*p.CacheWrite1h
	} else {
		usd += float64(u.CacheWrite) * p.CacheWrite5m
		c.AssumedTTLTokens = u.CacheWrite
	}
	c.USD = usd / million
	return c
}
