// Package report turns collected sessions into the comparison matrix: totals
// per QAM cohort, per environment and per model, priced from the table, with
// every assumption the numbers rest on counted beside them.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/atvirokodosprendimai/statsv1/internal/pricing"
	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

// Options narrows the sessions a report covers.
type Options struct {
	Since time.Time
	Until time.Time
}

func (o Options) keep(s usage.Session) bool {
	if !o.Since.IsZero() && s.StartedAt.Before(o.Since) {
		return false
	}
	if !o.Until.IsZero() && !s.StartedAt.Before(o.Until) {
		return false
	}
	return true
}

// Row is one line of the matrix: the sessions sharing a key and their totals.
type Row struct {
	Key              string  `json:"key"`
	Sessions         int     `json:"sessions"`
	UserTurns        int     `json:"user_turns"`
	Requests         int     `json:"requests"`
	ToolCalls        int     `json:"tool_calls"`
	Input            int64   `json:"input_tokens"`
	Output           int64   `json:"output_tokens"`
	CacheRead        int64   `json:"cache_read_tokens"`
	CacheWrite       int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	UnpricedRequests int     `json:"unpriced_requests"`
	AssumedTTLTokens int64   `json:"assumed_ttl_tokens"`
	UnverifiedUSD    float64 `json:"unverified_price_usd"`
}

// CostPerTurn is the cost of one human turn, the unit a person experiences.
func (r Row) CostPerTurn() float64 {
	if r.UserTurns == 0 {
		return 0
	}
	return r.CostUSD / float64(r.UserTurns)
}

// CostPerRequest is the cost of one API call.
func (r Row) CostPerRequest() float64 {
	if r.Requests == 0 {
		return 0
	}
	return r.CostUSD / float64(r.Requests)
}

// CacheHitRatio is the share of prompt tokens served from cache.
func (r Row) CacheHitRatio() float64 {
	denom := r.Input + r.CacheRead + r.CacheWrite
	if denom == 0 {
		return 0
	}
	return float64(r.CacheRead) / float64(denom)
}

func (r *Row) addSession(s usage.Session) {
	r.Sessions++
	r.UserTurns += s.UserTurns
	r.ToolCalls += s.ToolCalls
}

func (r *Row) addRequest(req usage.Request, c pricing.Cost) {
	r.Requests++
	r.Input += req.Tokens.Input
	r.Output += req.Tokens.Output
	r.CacheRead += req.Tokens.CacheRead
	r.CacheWrite += req.Tokens.CacheWrite
	r.CostUSD += c.USD
	r.AssumedTTLTokens += c.AssumedTTLTokens
	if !c.Priced {
		r.UnpricedRequests++
	} else if !c.Verified {
		r.UnverifiedUSD += c.USD
	}
}

// Matrix is the report.
type Matrix struct {
	GeneratedAt   time.Time  `json:"generated_at"`
	Since         *time.Time `json:"since,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
	PriceSource   string     `json:"price_source"`
	ByCohort      []Row      `json:"by_cohort"`
	ByEnvironment []Row      `json:"by_environment"`
	ByModel       []Row      `json:"by_model"`
	Total         Row        `json:"total"`
}

// Build aggregates sessions into the matrix.
func Build(sessions []usage.Session, envs map[string]usage.Environment, prices *pricing.Table, opt Options) *Matrix {
	m := &Matrix{GeneratedAt: time.Now(), PriceSource: prices.Source + " (as of " + prices.AsOf + ")", Total: Row{Key: "total"}}
	if !opt.Since.IsZero() {
		m.Since = &opt.Since
	}
	if !opt.Until.IsZero() {
		m.Until = &opt.Until
	}
	byCohort := map[string]*Row{}
	byEnv := map[string]*Row{}
	byModel := map[string]*Row{}
	get := func(set map[string]*Row, key string) *Row {
		if r, ok := set[key]; ok {
			return r
		}
		r := &Row{Key: key}
		set[key] = r
		return r
	}
	for _, s := range sessions {
		if !opt.keep(s) {
			continue
		}
		envLabel := "unknown"
		if e, ok := envs[s.ConfigDir]; ok {
			envLabel = e.Label()
		}
		cohort := get(byCohort, s.Signals.Cohort())
		env := get(byEnv, envLabel)
		cohort.addSession(s)
		env.addSession(s)
		m.Total.addSession(s)
		for _, req := range s.Requests {
			c := prices.Cost(req.Model, req.Tokens)
			cohort.addRequest(req, c)
			env.addRequest(req, c)
			m.Total.addRequest(req, c)
			get(byModel, pricing.Normalize(req.Model)).addRequest(req, c)
		}
	}
	m.ByCohort = sortedRows(byCohort, cohortOrder)
	m.ByEnvironment = sortedRows(byEnv, func(a, b string) bool { return a < b })
	m.ByModel = sortedRows(byModel, func(a, b string) bool { return byModel[a].CostUSD > byModel[b].CostUSD })
	return m
}

// cohortOrder puts the full stack first, then the partial cohorts, then none.
func cohortOrder(a, b string) bool {
	rank := func(k string) int {
		switch {
		case k == "QAM":
			return 0
		case strings.HasPrefix(k, "partial:"):
			return 1
		}
		return 2
	}
	if rank(a) != rank(b) {
		return rank(a) < rank(b)
	}
	return a < b
}

func sortedRows(set map[string]*Row, less func(a, b string) bool) []Row {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	rows := make([]Row, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, *set[k])
	}
	return rows
}

var columns = []string{"key", "sessions", "turns", "requests", "tool_calls", "input", "output", "cache_read", "cache_write", "cost_usd", "usd_per_turn", "usd_per_request", "cache_hit", "unpriced_requests", "assumed_ttl_tokens", "unverified_usd"}

func (r Row) values() []string {
	return []string{r.Key, strconv.Itoa(r.Sessions), strconv.Itoa(r.UserTurns), strconv.Itoa(r.Requests), strconv.Itoa(r.ToolCalls),
		strconv.FormatInt(r.Input, 10), strconv.FormatInt(r.Output, 10), strconv.FormatInt(r.CacheRead, 10), strconv.FormatInt(r.CacheWrite, 10),
		fmt.Sprintf("%.4f", r.CostUSD), fmt.Sprintf("%.4f", r.CostPerTurn()), fmt.Sprintf("%.5f", r.CostPerRequest()), fmt.Sprintf("%.3f", r.CacheHitRatio()),
		strconv.Itoa(r.UnpricedRequests), strconv.FormatInt(r.AssumedTTLTokens, 10), fmt.Sprintf("%.4f", r.UnverifiedUSD)}
}

// WriteCSV writes every section as one CSV with a leading "section" column.
func (m *Matrix) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(append([]string{"section"}, columns...)); err != nil {
		return err
	}
	sections := []struct {
		name string
		rows []Row
	}{{"cohort", m.ByCohort}, {"environment", m.ByEnvironment}, {"model", m.ByModel}, {"total", []Row{m.Total}}}
	for _, sec := range sections {
		for _, r := range sec.rows {
			if err := cw.Write(append([]string{sec.name}, r.values()...)); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteJSON writes the matrix as indented JSON.
func (m *Matrix) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// WriteText writes the matrix as aligned tables for a terminal.
func (m *Matrix) WriteText(w io.Writer) error {
	span := "all sessions"
	if m.Since != nil || m.Until != nil {
		span = "sessions started"
		if m.Since != nil {
			span += " from " + m.Since.Format("2006-01-02")
		}
		if m.Until != nil {
			span += " before " + m.Until.Format("2006-01-02")
		}
	}
	fmt.Fprintf(w, "statsv1 matrix, %s, generated %s\n", span, m.GeneratedAt.Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "prices: %s\n\n", m.PriceSource)
	sections := []struct {
		title string
		rows  []Row
	}{
		{"by QAM usage cohort (each session labelled by the tool calls in its own transcripts: Q = quality-harness, A = agentsmemory, M = mrw)", m.ByCohort},
		{"by environment (what the config directory had installed when collected)", m.ByEnvironment},
		{"by model", m.ByModel},
		{"total", []Row{m.Total}},
	}
	for _, sec := range sections {
		fmt.Fprintln(w, sec.title)
		if err := writeTable(w, sec.rows); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "reading the numbers:")
	fmt.Fprintln(w, "  usd_per_turn divides cost by human turns, usd_per_request by API calls; cache_hit is cache_read over all prompt tokens.")
	fmt.Fprintln(w, "  unpriced_requests are requests whose model is not in the price table (their tokens are counted, their cost is 0).")
	fmt.Fprintln(w, "  assumed_ttl_tokens are cache-write tokens priced at the 5-minute rate because the transcript did not record the TTL.")
	fmt.Fprintln(w, "  unverified_usd is cost computed from price rows recalled from memory rather than a documented table.")
	fmt.Fprintln(w, "  cohorts compare different sessions doing different work; a cheaper cohort is a fact about those sessions, not a controlled experiment.")
	return nil
}

func writeTable(w io.Writer, rows []Row) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(columns, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r.values(), "\t"))
	}
	return tw.Flush()
}

// SessionRow is one session priced and labelled, for the sessions listing.
type SessionRow struct {
	ID          string       `json:"session_id"`
	ConfigDir   string       `json:"config_dir"`
	Environment string       `json:"environment"`
	Project     string       `json:"project"`
	StartedAt   time.Time    `json:"started_at"`
	Duration    string       `json:"duration"`
	Cohort      string       `json:"cohort"`
	AM          int          `json:"am_calls"`
	MRW         int          `json:"mrw_calls"`
	QH          int          `json:"qh_calls"`
	UserTurns   int          `json:"user_turns"`
	Requests    int          `json:"requests"`
	Subagents   int          `json:"subagents"`
	Tokens      usage.Tokens `json:"tokens"`
	CostUSD     float64      `json:"cost_usd"`
	Models      []string     `json:"models"`
}

// SessionRows prices every session, newest first.
func SessionRows(sessions []usage.Session, envs map[string]usage.Environment, prices *pricing.Table, opt Options) []SessionRow {
	var out []SessionRow
	for _, s := range sessions {
		if !opt.keep(s) {
			continue
		}
		row := SessionRow{ID: s.ID, ConfigDir: s.ConfigDir, Environment: "unknown", Project: s.CWD, StartedAt: s.StartedAt,
			Cohort: s.Signals.Cohort(), AM: s.Signals.AM, MRW: s.Signals.MRW, QH: s.Signals.QH, UserTurns: s.UserTurns, Subagents: s.Subagents}
		if row.Project == "" {
			row.Project = s.ProjectSlug
		}
		if e, ok := envs[s.ConfigDir]; ok {
			row.Environment = e.Label()
		}
		if !s.EndedAt.IsZero() && !s.StartedAt.IsZero() {
			row.Duration = s.EndedAt.Sub(s.StartedAt).Round(time.Minute).String()
		}
		models := map[string]bool{}
		for _, req := range s.Requests {
			row.Requests++
			row.Tokens.Add(req.Tokens)
			row.CostUSD += prices.Cost(req.Model, req.Tokens).USD
			models[pricing.Normalize(req.Model)] = true
		}
		for k := range models {
			row.Models = append(row.Models, k)
		}
		sort.Strings(row.Models)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// WriteSessionsText writes the session listing as an aligned table.
func WriteSessionsText(w io.Writer, rows []SessionRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "started\tsession\tcohort\tQ\tA\tM\tturns\trequests\tsubagents\tinput\toutput\tcache_read\tcache_write\tcost_usd\tenvironment\tproject\tmodels")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.4f\t%s\t%s\t%s\n",
			r.StartedAt.Format("2006-01-02 15:04"), shortID(r.ID), r.Cohort, r.QH, r.AM, r.MRW, r.UserTurns, r.Requests, r.Subagents,
			r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheRead, r.Tokens.CacheWrite, r.CostUSD, r.Environment, r.Project, strings.Join(r.Models, ","))
	}
	return tw.Flush()
}

// WriteSessionsCSV writes the session listing as CSV.
func WriteSessionsCSV(w io.Writer, rows []SessionRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"started", "session_id", "config_dir", "environment", "project", "duration", "cohort", "qh_calls", "am_calls", "mrw_calls", "user_turns", "requests", "subagents", "input", "output", "cache_read", "cache_write", "cost_usd", "models"}); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{r.StartedAt.Format(time.RFC3339), r.ID, r.ConfigDir, r.Environment, r.Project, r.Duration, r.Cohort,
			strconv.Itoa(r.QH), strconv.Itoa(r.AM), strconv.Itoa(r.MRW), strconv.Itoa(r.UserTurns), strconv.Itoa(r.Requests), strconv.Itoa(r.Subagents),
			strconv.FormatInt(r.Tokens.Input, 10), strconv.FormatInt(r.Tokens.Output, 10), strconv.FormatInt(r.Tokens.CacheRead, 10), strconv.FormatInt(r.Tokens.CacheWrite, 10),
			fmt.Sprintf("%.4f", r.CostUSD), strings.Join(r.Models, " ")}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
