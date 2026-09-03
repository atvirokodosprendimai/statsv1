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
// Days counts the calendar days on which one of them started, so a period
// can be normalised by how much of it was actually used.
type Row struct {
	Key                string  `json:"key"`
	Sessions           int     `json:"sessions"`
	Days               int     `json:"days"`
	UserTurns          int     `json:"user_turns"`
	Requests           int     `json:"requests"`
	ToolCalls          int     `json:"tool_calls"`
	Input              int64   `json:"input_tokens"`
	Output             int64   `json:"output_tokens"`
	Thinking           int64   `json:"thinking_tokens"`
	CacheRead          int64   `json:"cache_read_tokens"`
	CacheWrite         int64   `json:"cache_write_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	UnpricedRequests   int     `json:"unpriced_requests"`
	AssumedTTLTokens   int64   `json:"assumed_ttl_tokens"`
	UnverifiedUSD      float64 `json:"unverified_price_usd"`
	ThinkingUnrecorded int     `json:"thinking_unrecorded_requests"`
	days               map[string]struct{}
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

// CostPerDay divides cost by the calendar days on which a session started.
func (r Row) CostPerDay() float64 {
	if r.Days == 0 {
		return 0
	}
	return r.CostUSD / float64(r.Days)
}

func (r *Row) addSession(s usage.Session) {
	r.Sessions++
	r.UserTurns += s.UserTurns
	r.ToolCalls += s.ToolCalls
	if !s.StartedAt.IsZero() {
		if r.days == nil {
			r.days = map[string]struct{}{}
		}
		r.days[s.StartedAt.UTC().Format("2006-01-02")] = struct{}{}
		r.Days = len(r.days)
	}
}

func (r *Row) addRequest(req usage.Request, c pricing.Cost) {
	r.Requests++
	r.Input += req.Tokens.Input
	r.Output += req.Tokens.Output
	r.Thinking += req.Tokens.Thinking
	if !req.Tokens.ThinkingKnown {
		r.ThinkingUnrecorded++
	}
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

var columns = []string{"key", "sessions", "days", "turns", "requests", "tool_calls", "input", "output", "thinking", "cache_read", "cache_write", "cost_usd", "usd_per_turn", "usd_per_request", "usd_per_day", "cache_hit", "unpriced_requests", "assumed_ttl_tokens", "unverified_usd", "thinking_unrecorded"}

func (r Row) values() []string {
	return []string{r.Key, strconv.Itoa(r.Sessions), strconv.Itoa(r.Days), strconv.Itoa(r.UserTurns), strconv.Itoa(r.Requests), strconv.Itoa(r.ToolCalls),
		strconv.FormatInt(r.Input, 10), strconv.FormatInt(r.Output, 10), strconv.FormatInt(r.Thinking, 10), strconv.FormatInt(r.CacheRead, 10), strconv.FormatInt(r.CacheWrite, 10),
		fmt.Sprintf("%.4f", r.CostUSD), fmt.Sprintf("%.4f", r.CostPerTurn()), fmt.Sprintf("%.5f", r.CostPerRequest()), fmt.Sprintf("%.4f", r.CostPerDay()), fmt.Sprintf("%.3f", r.CacheHitRatio()),
		strconv.Itoa(r.UnpricedRequests), strconv.FormatInt(r.AssumedTTLTokens, 10), fmt.Sprintf("%.4f", r.UnverifiedUSD), strconv.Itoa(r.ThinkingUnrecorded)}
}

func getRow(set map[string]*Row, key string) *Row {
	if r, ok := set[key]; ok {
		return r
	}
	r := &Row{Key: key}
	set[key] = r
	return r
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

// FirstUse records when each QAM component first appears in the data: the
// start of the earliest session whose own tool calls show it.
type FirstUse struct {
	Agentsmemory   time.Time `json:"agentsmemory"`
	QualityHarness time.Time `json:"quality_harness"`
	MRW            time.Time `json:"mrw"`
}

// FirstUses scans the sessions for the first use of each component.
func FirstUses(sessions []usage.Session) FirstUse {
	var f FirstUse
	earlier := func(cur *time.Time, at time.Time) {
		if cur.IsZero() || at.Before(*cur) {
			*cur = at
		}
	}
	for _, s := range sessions {
		if s.StartedAt.IsZero() {
			continue
		}
		if s.Signals.AM > 0 {
			earlier(&f.Agentsmemory, s.StartedAt)
		}
		if s.Signals.QH > 0 {
			earlier(&f.QualityHarness, s.StartedAt)
		}
		if s.Signals.MRW > 0 {
			earlier(&f.MRW, s.StartedAt)
		}
	}
	return f
}

// Installed records where and when the quality-harness plugin was installed.
type Installed struct {
	Dir string    `json:"dir"`
	At  time.Time `json:"at"`
}

// Comparison is the before/after view around a split date, with the time
// buckets in between (month, week or day) so a change can be placed in time,
// the per-request trend with its change bucket to bucket, and the cohorts
// inside each period so a change in cost can be told from a change in what
// was used.
type Comparison struct {
	GeneratedAt   time.Time   `json:"generated_at"`
	SplitAt       time.Time   `json:"split_at"`
	SplitReason   string      `json:"split_reason"`
	Bucket        string      `json:"bucket"`
	FirstUse      FirstUse    `json:"first_use"`
	Installed     []Installed `json:"quality_harness_installed"`
	PriceSource   string      `json:"price_source"`
	Before        Row         `json:"before"`
	After         Row         `json:"after"`
	ByPeriod      []Row       `json:"by_period"`
	Trend         []Trend     `json:"trend"`
	CohortsBefore []Row       `json:"cohorts_before"`
	CohortsAfter  []Row       `json:"cohorts_after"`
}

// bucketKey names the bucket a start time falls in. Weeks are ISO weeks and
// carry their Monday so the key reads without a calendar; keys sort as text.
func bucketKey(t time.Time, bucket string) string {
	t = t.UTC()
	switch bucket {
	case "day":
		return t.Format("2006-01-02")
	case "week":
		year, week := t.ISOWeek()
		monday := t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
		return fmt.Sprintf("%d-W%02d (%s)", year, week, monday.Format("01-02"))
	}
	return t.Format("2006-01")
}

// Compare aggregates the sessions started before split and from split on,
// and buckets every session by month, week or day.
func Compare(sessions []usage.Session, envs map[string]usage.Environment, prices *pricing.Table, split time.Time, reason, bucket string) *Comparison {
	day := split.Format("2006-01-02")
	c := &Comparison{
		GeneratedAt: time.Now(), SplitAt: split, SplitReason: reason, Bucket: bucket, FirstUse: FirstUses(sessions),
		PriceSource: prices.Source + " (as of " + prices.AsOf + ")",
		Before:      Row{Key: "before " + day},
		After:       Row{Key: "from " + day},
	}
	for _, e := range envs {
		if !e.QualityHarnessInstalledAt.IsZero() {
			c.Installed = append(c.Installed, Installed{Dir: e.Dir, At: e.QualityHarnessInstalledAt})
		}
	}
	sort.Slice(c.Installed, func(i, j int) bool { return c.Installed[i].At.Before(c.Installed[j].At) })
	byPeriod := map[string]*Row{}
	before := map[string]*Row{}
	after := map[string]*Row{}
	for _, s := range sessions {
		if s.StartedAt.IsZero() {
			continue
		}
		period, cohorts := &c.After, after
		if s.StartedAt.Before(split) {
			period, cohorts = &c.Before, before
		}
		slot := getRow(byPeriod, bucketKey(s.StartedAt, bucket))
		cohort := getRow(cohorts, s.Signals.Cohort())
		period.addSession(s)
		slot.addSession(s)
		cohort.addSession(s)
		for _, req := range s.Requests {
			cost := prices.Cost(req.Model, req.Tokens)
			period.addRequest(req, cost)
			slot.addRequest(req, cost)
			cohort.addRequest(req, cost)
		}
	}
	c.ByPeriod = sortedRows(byPeriod, func(a, b string) bool { return a < b })
	c.Trend = Trends(c.ByPeriod)
	c.CohortsBefore = sortedRows(before, cohortOrder)
	c.CohortsAfter = sortedRows(after, cohortOrder)
	return c
}

// Trend is one time bucket read per request and per turn, with the change of
// every figure against the previous bucket, so what grew and what shrank
// reads directly.
type Trend struct {
	Key                  string  `json:"key"`
	Sessions             int     `json:"sessions"`
	Requests             int     `json:"requests"`
	UserTurns            int     `json:"user_turns"`
	ToolCalls            int     `json:"tool_calls"`
	CostUSD              float64 `json:"cost_usd"`
	USDPerRequest        float64 `json:"usd_per_request"`
	USDPerTurn           float64 `json:"usd_per_turn"`
	InputPerRequest      float64 `json:"input_per_request"`
	OutputPerRequest     float64 `json:"output_per_request"`
	ThinkingPerRequest   float64 `json:"thinking_per_request"`
	ThinkingShare        float64 `json:"thinking_share_of_output"`
	CacheReadPerRequest  float64 `json:"cache_read_per_request"`
	CacheWritePerRequest float64 `json:"cache_write_per_request"`
	RequestsPerTurn      float64 `json:"requests_per_turn"`
	ToolCallsPerRequest  float64 `json:"tool_calls_per_request"`
	// Change is the percentage change of each figure against the previous
	// bucket, keyed by the figure's name. Absent on the first bucket, and
	// for a figure whose previous value was zero.
	Change map[string]float64 `json:"change_pct,omitempty"`
}

// trendMetrics lists the figures a trend row carries, in print order, with
// the format each is printed in.
var trendMetrics = []struct {
	name   string
	format string
	get    func(t Trend) float64
}{
	{"requests", "%.0f", func(t Trend) float64 { return float64(t.Requests) }},
	{"usd_per_request", "%.4f", func(t Trend) float64 { return t.USDPerRequest }},
	{"cost_usd", "%.2f", func(t Trend) float64 { return t.CostUSD }},
	{"turns", "%.0f", func(t Trend) float64 { return float64(t.UserTurns) }},
	{"requests_per_turn", "%.2f", func(t Trend) float64 { return t.RequestsPerTurn }},
	{"usd_per_turn", "%.3f", func(t Trend) float64 { return t.USDPerTurn }},
	{"sessions", "%.0f", func(t Trend) float64 { return float64(t.Sessions) }},
	{"input_per_request", "%.1f", func(t Trend) float64 { return t.InputPerRequest }},
	{"output_per_request", "%.0f", func(t Trend) float64 { return t.OutputPerRequest }},
	{"thinking_per_request", "%.0f", func(t Trend) float64 { return t.ThinkingPerRequest }},
	{"thinking_share_of_output", "%.3f", func(t Trend) float64 { return t.ThinkingShare }},
	{"cache_read_per_request", "%.0f", func(t Trend) float64 { return t.CacheReadPerRequest }},
	{"cache_write_per_request", "%.0f", func(t Trend) float64 { return t.CacheWritePerRequest }},
	{"tool_calls_per_request", "%.2f", func(t Trend) float64 { return t.ToolCallsPerRequest }},
}

func ratioOf(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}

// Trends derives the per-request view of each bucket and the change of every
// figure against the bucket before it.
func Trends(rows []Row) []Trend {
	out := make([]Trend, 0, len(rows))
	for _, r := range rows {
		requests := float64(r.Requests)
		t := Trend{
			Key: r.Key, Sessions: r.Sessions, Requests: r.Requests, UserTurns: r.UserTurns, ToolCalls: r.ToolCalls, CostUSD: r.CostUSD,
			USDPerRequest:        r.CostPerRequest(),
			USDPerTurn:           r.CostPerTurn(),
			InputPerRequest:      ratioOf(float64(r.Input), requests),
			OutputPerRequest:     ratioOf(float64(r.Output), requests),
			ThinkingPerRequest:   ratioOf(float64(r.Thinking), requests),
			ThinkingShare:        ratioOf(float64(r.Thinking), float64(r.Output)),
			CacheReadPerRequest:  ratioOf(float64(r.CacheRead), requests),
			CacheWritePerRequest: ratioOf(float64(r.CacheWrite), requests),
			RequestsPerTurn:      ratioOf(requests, float64(r.UserTurns)),
			ToolCallsPerRequest:  ratioOf(float64(r.ToolCalls), requests),
		}
		if n := len(out); n > 0 {
			prev := out[n-1]
			t.Change = map[string]float64{}
			for _, m := range trendMetrics {
				if before := m.get(prev); before != 0 {
					t.Change[m.name] = (m.get(t) - before) / before * 100
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// cell prints one trend figure with its change against the previous bucket.
func (t Trend) cell(name, format string, v float64) string {
	s := fmt.Sprintf(format, v)
	if pct, ok := t.Change[name]; ok {
		return fmt.Sprintf("%s (%+.0f%%)", s, pct)
	}
	return s
}

func writeTrendTable(w io.Writer, trend []Trend, bucket string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{bucket}
	for _, m := range trendMetrics {
		header = append(header, m.name)
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, t := range trend {
		cells := []string{t.Key}
		for _, m := range trendMetrics {
			cells = append(cells, t.cell(m.name, m.format, m.get(t)))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// Changes lists the after-to-before ratios that answer "what changed". The
// first line is the identity the rest hang on: what a human turn costs is the
// number of requests it drives times what a request costs, so fewer, dearer
// requests can be a saving and more, cheaper ones an expense.
func (c *Comparison) Changes() []string {
	b, a := c.Before, c.After
	perTurn := func(n, turns int) float64 {
		if turns == 0 {
			return 0
		}
		return float64(n) / float64(turns)
	}
	perDay := func(n, days int) float64 {
		if days == 0 {
			return 0
		}
		return float64(n) / float64(days)
	}
	perRequest := func(n int64, requests int) float64 {
		if requests == 0 {
			return 0
		}
		return float64(n) / float64(requests)
	}
	share := func(part, whole int64) float64 {
		if whole == 0 {
			return 0
		}
		return float64(part) / float64(whole)
	}
	return []string{
		fmt.Sprintf("cost per human turn = requests per turn x cost per request: %s x %s = %s",
			factor(perTurn(a.Requests, a.UserTurns), perTurn(b.Requests, b.UserTurns)), factor(a.CostPerRequest(), b.CostPerRequest()), factor(a.CostPerTurn(), b.CostPerTurn())),
		"requests per human turn: " + ratio(perTurn(a.Requests, a.UserTurns), perTurn(b.Requests, b.UserTurns)),
		"cost per request: " + ratio(a.CostPerRequest(), b.CostPerRequest()),
		"cost per human turn: " + ratio(a.CostPerTurn(), b.CostPerTurn()),
		"requests per active day: " + ratio(perDay(a.Requests, a.Days), perDay(b.Requests, b.Days)),
		"cost per active day: " + ratio(a.CostPerDay(), b.CostPerDay()),
		"sessions per active day: " + ratio(perDay(a.Sessions, a.Days), perDay(b.Sessions, b.Days)),
		"tool calls per human turn: " + ratio(perTurn(a.ToolCalls, a.UserTurns), perTurn(b.ToolCalls, b.UserTurns)),
		"output tokens per request: " + ratio(perRequest(a.Output, a.Requests), perRequest(b.Output, b.Requests)),
		"thinking tokens per request: " + ratio(perRequest(a.Thinking, a.Requests), perRequest(b.Thinking, b.Requests)),
		fmt.Sprintf("thinking share of output: %.3f before, %.3f after (%d and %d requests carry no thinking counter)", share(b.Thinking, b.Output), share(a.Thinking, a.Output), b.ThinkingUnrecorded, a.ThinkingUnrecorded),
		"cache read tokens per request: " + ratio(perRequest(a.CacheRead, a.Requests), perRequest(b.CacheRead, b.Requests)),
		fmt.Sprintf("cache hit ratio: %.3f before, %.3f after", b.CacheHitRatio(), a.CacheHitRatio()),
	}
}

// factor prints an after-over-before multiplier on its own.
func factor(after, before float64) string {
	if before == 0 || after == 0 {
		return "n/a"
	}
	return fmt.Sprintf("x%.2f", after/before)
}

func ratio(after, before float64) string {
	if before == 0 || after == 0 {
		return fmt.Sprintf("not comparable (before %.4f, after %.4f)", before, after)
	}
	return fmt.Sprintf("x%.2f (before %.4f, after %.4f)", after/before, before, after)
}

func dateOrNone(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02")
}

// WriteText writes the comparison as aligned tables for a terminal.
func (c *Comparison) WriteText(w io.Writer) error {
	day := c.SplitAt.Format("2006-01-02")
	fmt.Fprintf(w, "statsv1 compare: sessions started before %s against sessions started from that day on (split = %s), generated %s\n", day, c.SplitReason, c.GeneratedAt.Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "first session using each component: agentsmemory %s, quality-harness %s, mrw %s\n", dateOrNone(c.FirstUse.Agentsmemory), dateOrNone(c.FirstUse.QualityHarness), dateOrNone(c.FirstUse.MRW))
	for _, i := range c.Installed {
		fmt.Fprintf(w, "quality-harness plugin installed in %s on %s\n", i.Dir, i.At.UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(w, "prices: %s\n\n", c.PriceSource)
	sections := []struct {
		title string
		rows  []Row
	}{
		{"before and after", []Row{c.Before, c.After}},
		{"by " + c.Bucket, c.ByPeriod},
		{"by QAM usage cohort, before " + day, c.CohortsBefore},
		{"by QAM usage cohort, from " + day, c.CohortsAfter},
	}
	for _, sec := range sections {
		fmt.Fprintln(w, sec.title)
		if err := writeTable(w, sec.rows); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "per request and per turn by %s, each figure with its change against the previous %s (nothing to compare on the first):\n", c.Bucket, c.Bucket)
	if err := writeTrendTable(w, c.Trend, c.Bucket); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "what changed (after relative to before):")
	for _, line := range c.Changes() {
		fmt.Fprintln(w, "  "+line)
	}
	fmt.Fprintln(w, "  days counts calendar days with at least one session start; the two periods hold different work on different days, so this is a time series, not an experiment.")
	fmt.Fprintln(w, "  every figure is transcript-recorded usage, a lower bound by the same mechanism in both periods (see verify).")
	return nil
}

// WriteCSV writes every section as one CSV with a leading "section" column.
func (c *Comparison) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(append([]string{"section"}, columns...)); err != nil {
		return err
	}
	sections := []struct {
		name string
		rows []Row
	}{{"period", []Row{c.Before, c.After}}, {c.Bucket, c.ByPeriod}, {"cohort_before", c.CohortsBefore}, {"cohort_after", c.CohortsAfter}}
	for _, sec := range sections {
		for _, r := range sec.rows {
			if err := cw.Write(append([]string{sec.name}, r.values()...)); err != nil {
				return err
			}
		}
	}
	// The trend has its own columns, so it follows as a second block with its
	// own header rather than being forced into the matrix columns.
	header := []string{"trend_" + c.Bucket}
	for _, m := range trendMetrics {
		header = append(header, m.name, m.name+"_change_pct")
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, t := range c.Trend {
		rec := []string{t.Key}
		for _, m := range trendMetrics {
			rec = append(rec, fmt.Sprintf(m.format, m.get(t)))
			if pct, ok := t.Change[m.name]; ok {
				rec = append(rec, fmt.Sprintf("%.1f", pct))
			} else {
				rec = append(rec, "")
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteJSON writes the comparison as indented JSON.
func (c *Comparison) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}
