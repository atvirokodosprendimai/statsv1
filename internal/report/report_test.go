package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/statsv1/internal/pricing"
	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

func fixture() ([]usage.Session, map[string]usage.Environment) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	req := func(id, model string, out int64) usage.Request {
		return usage.Request{MessageID: id, At: day, Model: model, Tokens: usage.Tokens{Input: 100, Output: out, Thinking: out / 4, ThinkingKnown: true, CacheRead: 1000, CacheWrite: 10, CacheWrite1h: 10, TTLKnown: true}}
	}
	sessions := []usage.Session{
		{ID: "qam", ConfigDir: "/sandbox", StartedAt: day, UserTurns: 2, ToolCalls: 5, Signals: usage.Signals{AM: 2, MRW: 1, QH: 1},
			Requests: []usage.Request{req("m1", "claude-opus-5", 1000), req("m2", "claude-opus-5", 1000)}},
		{ID: "plain", ConfigDir: "/host", StartedAt: day.AddDate(0, 0, 1), UserTurns: 1, ToolCalls: 1,
			Requests: []usage.Request{req("m3", "claude-opus-5", 4000), req("m4", "<synthetic>", 0)}},
		{ID: "partial", ConfigDir: "/host", StartedAt: day.AddDate(0, 0, 2), UserTurns: 1, Signals: usage.Signals{AM: 1},
			Requests: []usage.Request{req("m5", "claude-sonnet-4-5-20250929", 100)}},
	}
	envs := map[string]usage.Environment{
		"/sandbox": {Dir: "/sandbox", HasAgentsmemory: true, HasQualityHarness: true},
		"/host":    {Dir: "/host"},
	}
	return sessions, envs
}

func TestBuildGroupsByCohortEnvironmentAndModel(t *testing.T) {
	prices, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	sessions, envs := fixture()
	m := Build(sessions, envs, prices, Options{})

	keys := func(rows []Row) []string {
		var ks []string
		for _, r := range rows {
			ks = append(ks, r.Key)
		}
		return ks
	}
	if got := strings.Join(keys(m.ByCohort), ","); got != "QAM,partial:A,none" {
		t.Errorf("cohort order = %s, want QAM,partial:A,none", got)
	}
	qam := m.ByCohort[0]
	if qam.Sessions != 1 || qam.UserTurns != 2 || qam.Requests != 2 || qam.Output != 2000 || qam.Thinking != 500 || qam.ToolCalls != 5 {
		t.Errorf("QAM row = %+v", qam)
	}
	// 2 requests x (100 in x 5 + 1000 out x 25 + 1000 cr x 0.5 + 10 cw1h x 10) per million.
	wantQAM := 2 * (100*5 + 1000*25 + 1000*0.5 + 10*10) / 1e6
	if math.Abs(qam.CostUSD-wantQAM) > 1e-9 {
		t.Errorf("QAM cost = %.6f, want %.6f", qam.CostUSD, wantQAM)
	}
	if math.Abs(qam.CostPerTurn()-wantQAM/2) > 1e-9 {
		t.Errorf("QAM cost per turn = %.6f, want %.6f", qam.CostPerTurn(), wantQAM/2)
	}
	none := m.ByCohort[2]
	if none.UnpricedRequests != 1 || none.Requests != 2 {
		t.Errorf("synthetic request must be counted but unpriced: %+v", none)
	}
	partial := m.ByCohort[1]
	if partial.UnverifiedUSD <= 0 || math.Abs(partial.UnverifiedUSD-partial.CostUSD) > 1e-12 {
		t.Errorf("sonnet-4-5 is priced from memory, so its whole cost is unverified: %+v", partial)
	}
	if got := strings.Join(keys(m.ByEnvironment), ","); got != "plain,qam-installed" {
		t.Errorf("environment keys = %s", got)
	}
	if m.ByModel[0].Key != "claude-opus-5" {
		t.Errorf("most expensive model should lead: %v", keys(m.ByModel))
	}
	if m.Total.Sessions != 3 || m.Total.Requests != 5 {
		t.Errorf("total = %+v", m.Total)
	}
	if hit := m.Total.CacheHitRatio(); hit <= 0.85 || hit >= 0.95 {
		t.Errorf("cache hit ratio = %.3f, want about 0.9 (1000 of 1110 prompt tokens)", hit)
	}
}

func TestOptionsSinceUntilFilterBySessionStart(t *testing.T) {
	prices, _ := pricing.Load()
	sessions, envs := fixture()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	m := Build(sessions, envs, prices, Options{Since: day.AddDate(0, 0, 1), Until: day.AddDate(0, 0, 2)})
	if m.Total.Sessions != 1 || m.ByCohort[0].Key != "none" {
		t.Errorf("filter kept the wrong sessions: %+v", m.ByCohort)
	}
}

func TestWritersProduceEverySection(t *testing.T) {
	prices, _ := pricing.Load()
	sessions, envs := fixture()
	m := Build(sessions, envs, prices, Options{})

	var text bytes.Buffer
	if err := m.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"by QAM usage cohort", "by environment", "by model", "QAM", "partial:A", "none", "qam-installed", "claude-opus-5", "assumed_ttl_tokens"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, text.String())
		}
	}
	var csvOut bytes.Buffer
	if err := m.WriteCSV(&csvOut); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	// header + 3 cohorts + 2 environments + 3 models + total
	if len(lines) != 10 {
		t.Errorf("csv lines = %d, want 10:\n%s", len(lines), csvOut.String())
	}
	var js bytes.Buffer
	if err := m.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"by_cohort"`) {
		t.Errorf("json lacks by_cohort: %s", js.String())
	}
	rows := SessionRows(sessions, envs, prices, Options{})
	if len(rows) != 3 || rows[0].ID != "partial" || rows[2].Cohort != "QAM" {
		t.Errorf("session rows should be newest first: %+v", rows)
	}
	var st bytes.Buffer
	if err := WriteSessionsText(&st, rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.String(), "qam-installed") {
		t.Errorf("sessions text lacks environment column:\n%s", st.String())
	}
}

func TestCompareSplitsSessionsAtTheDateAndCountsDays(t *testing.T) {
	prices, _ := pricing.Load()
	sessions, envs := fixture()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first := FirstUses(sessions)
	if !first.QualityHarness.Equal(day) || !first.Agentsmemory.Equal(day) || !first.MRW.Equal(day) {
		t.Errorf("first uses = %+v, want all on %v", first, day)
	}
	c := Compare(sessions, envs, prices, day.AddDate(0, 0, 1), "test", "month")
	if c.Before.Sessions != 1 || c.Before.Days != 1 || c.After.Sessions != 2 || c.After.Days != 2 {
		t.Errorf("periods: before %+v, after %+v", c.Before, c.After)
	}
	if len(c.ByPeriod) != 1 || c.ByPeriod[0].Key != "2026-09" || c.ByPeriod[0].Sessions != 3 || c.ByPeriod[0].Days != 3 {
		t.Errorf("months = %+v", c.ByPeriod)
	}
	if len(c.CohortsBefore) != 1 || c.CohortsBefore[0].Key != "QAM" || len(c.CohortsAfter) != 2 {
		t.Errorf("cohorts before %+v, after %+v", c.CohortsBefore, c.CohortsAfter)
	}
	if got := c.After.CostPerDay(); math.Abs(got-c.After.CostUSD/2) > 1e-12 {
		t.Errorf("cost per day = %f, want cost over two days", got)
	}
	var text bytes.Buffer
	if err := c.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before 2026-09-02", "from 2026-09-02", "by month", "per request and per turn by month", "what changed", "cost per human turn", "usd_per_day"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("compare text lacks %q:\n%s", want, text.String())
		}
	}
	var csvOut bytes.Buffer
	if err := c.WriteCSV(&csvOut); err != nil {
		t.Fatal(err)
	}
	// header + 2 periods + 1 month + 1 cohort before + 2 cohorts after + trend header + 1 trend row
	if n := len(strings.Split(strings.TrimSpace(csvOut.String()), "\n")); n != 9 {
		t.Errorf("csv lines = %d, want 9:\n%s", n, csvOut.String())
	}
	var js bytes.Buffer
	if err := c.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"by_period"`) || !strings.Contains(js.String(), `"days"`) || !strings.Contains(js.String(), `"trend"`) {
		t.Errorf("json lacks by_period, days or trend: %s", js.String())
	}
}

func TestCompareBucketsByWeekAndDayWithChangeAgainstThePreviousBucket(t *testing.T) {
	prices, _ := pricing.Load()
	sessions, envs := fixture()
	split := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	week := Compare(sessions, envs, prices, split, "test", "week")
	// 2026-09-01 is a Tuesday: all three sessions fall in ISO week 36, whose Monday is 08-31.
	if len(week.ByPeriod) != 1 || week.ByPeriod[0].Key != "2026-W36 (08-31)" || week.ByPeriod[0].Sessions != 3 {
		t.Errorf("weeks = %+v", week.ByPeriod)
	}
	if len(week.Trend) != 1 || week.Trend[0].Change != nil {
		t.Errorf("a single bucket has nothing to change against: %+v", week.Trend)
	}
	days := Compare(sessions, envs, prices, split, "test", "day")
	if len(days.ByPeriod) != 3 || days.ByPeriod[0].Key != "2026-09-01" || days.ByPeriod[2].Key != "2026-09-03" {
		t.Errorf("days = %+v", days.ByPeriod)
	}
	tr := days.Trend
	// day 1: 2 requests; day 2: 2 requests (one of them synthetic); day 3: 1 request.
	if tr[0].Change != nil || math.Abs(tr[1].Change["requests"]) > 1e-9 || math.Abs(tr[2].Change["requests"]+50) > 1e-9 {
		t.Errorf("request change wrong: %+v %+v %+v", tr[0].Change, tr[1].Change, tr[2].Change)
	}
	if tr[2].Change["sessions"] != 0 || tr[1].Requests != 2 || tr[2].UserTurns != 1 {
		t.Errorf("trend rows wrong: %+v", tr)
	}
	var text bytes.Buffer
	if err := days.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "(-50%)") || !strings.Contains(text.String(), "per request and per turn by day") {
		t.Errorf("day trend text lacks the change marker:\n%s", text.String())
	}
}
