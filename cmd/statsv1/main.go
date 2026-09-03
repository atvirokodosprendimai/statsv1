// Command statsv1 collects Claude Code token usage from every config
// directory on this machine, stores it in SQLite, and reports it as a matrix
// that compares sessions by the QAM components (Quality Harness, AI Agent
// Memory, MultiPath Read/Write) they actually used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/statsv1/internal/claudecode"
	"github.com/atvirokodosprendimai/statsv1/internal/pricing"
	"github.com/atvirokodosprendimai/statsv1/internal/report"
	"github.com/atvirokodosprendimai/statsv1/internal/store"
	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "statsv1:", err)
		os.Exit(1)
	}
}

// Flags are built per command: urfave/cli flags carry parse state, so one
// instance must not be shared between commands.
func dbFlag() cli.Flag {
	return &cli.StringFlag{Name: "db", Value: "statsv1.db", Usage: "SQLite file the collector writes and the reports read", Sources: cli.EnvVars("STATSV1_DB")}
}

func formatFlag() cli.Flag {
	return &cli.StringFlag{Name: "format", Value: "table", Usage: "table, csv or json"}
}

func rangeFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "since", Usage: "only sessions started on or after this date (YYYY-MM-DD)"},
		&cli.StringFlag{Name: "until", Usage: "only sessions started before this date (YYYY-MM-DD)"},
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:  "statsv1",
		Usage: "Claude Code token usage and cost, with and without the QAM stack, from the transcripts on this machine",
		Commands: []*cli.Command{
			{
				Name:   "collect",
				Usage:  "scan Claude Code config directories and store every session's usage",
				Flags:  []cli.Flag{dbFlag(), &cli.StringSliceFlag{Name: "config-dir", Usage: "config directory to scan (repeatable); default: $CLAUDE_CONFIG_DIR, ~/.claude and every ~/.sandboxes/*"}},
				Action: runCollect,
			},
			{
				Name:   "report",
				Usage:  "print the comparison matrix by QAM cohort, environment and model",
				Flags:  append([]cli.Flag{dbFlag(), formatFlag()}, rangeFlags()...),
				Action: runReport,
			},
			{
				Name:  "sessions",
				Usage: "list sessions with their cohort, tokens and cost, newest first",
				Flags: append([]cli.Flag{dbFlag(), formatFlag(),
					&cli.StringFlag{Name: "cohort", Usage: "only this cohort (QAM, none, partial:A, ...)"},
					&cli.StringFlag{Name: "limit", Value: "50", Usage: "rows to print, 0 for all"},
				}, rangeFlags()...),
				Action: runSessions,
			},
			{
				Name:   "verify",
				Usage:  "compare the collector's cost with Claude Code's own lastCost for each project's last session",
				Flags:  []cli.Flag{dbFlag()},
				Action: runVerify,
			},
			{
				Name:  "compare",
				Usage: "sessions before a date against sessions from it on, by month, week or day, with what changed",
				Flags: []cli.Flag{dbFlag(), formatFlag(),
					&cli.StringFlag{Name: "at", Usage: "split date (YYYY-MM-DD); default: the day of the first session that used quality-harness"},
					&cli.StringFlag{Name: "by", Value: "month", Usage: "time table granularity: month, week or day"},
				},
				Action: runCompare,
			},
			{
				Name:   "prices",
				Usage:  "print the price table and its provenance",
				Action: runPrices,
			},
		},
	}
}

func runCollect(_ context.Context, c *cli.Command) error {
	dirs := c.StringSlice("config-dir")
	if len(dirs) == 0 {
		dirs = claudecode.DiscoverConfigDirs()
	}
	if len(dirs) == 0 {
		return errors.New("no Claude Code config directory with a projects/ folder found; pass --config-dir")
	}
	st, err := store.Open(c.String("db"))
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	for _, dir := range dirs {
		env := claudecode.ReadEnvironment(dir, now)
		if err := st.PutEnvironment(env); err != nil {
			return err
		}
		res, err := claudecode.ScanProjects(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		requests := 0
		for _, s := range res.Sessions {
			if err := st.PutSession(s); err != nil {
				return err
			}
			requests += len(s.Requests)
		}
		refs, err := claudecode.ReadCostReferences(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		if err := st.PutCostReferences(refs); err != nil {
			return err
		}
		fmt.Printf("%s: %s, %d files, %d sessions, %d requests, %d cost references, %d undecodable lines\n",
			dir, env.Label(), res.Files, len(res.Sessions), requests, len(refs), res.SkippedLines)
	}
	return nil
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q: want YYYY-MM-DD", s)
	}
	return t, nil
}

func options(c *cli.Command) (report.Options, error) {
	since, err := parseDate(c.String("since"))
	if err != nil {
		return report.Options{}, err
	}
	until, err := parseDate(c.String("until"))
	if err != nil {
		return report.Options{}, err
	}
	return report.Options{Since: since, Until: until}, nil
}

// load opens the store and reads everything a report needs.
func load(c *cli.Command) ([]usage.Session, map[string]usage.Environment, *pricing.Table, error) {
	prices, err := pricing.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := store.Open(c.String("db"))
	if err != nil {
		return nil, nil, nil, err
	}
	defer st.Close()
	sessions, err := st.Sessions()
	if err != nil {
		return nil, nil, nil, err
	}
	envs, err := st.Environments()
	if err != nil {
		return nil, nil, nil, err
	}
	if len(sessions) == 0 {
		return nil, nil, nil, errors.New("the database holds no sessions; run collect first")
	}
	return sessions, envs, prices, nil
}

func runReport(_ context.Context, c *cli.Command) error {
	opt, err := options(c)
	if err != nil {
		return err
	}
	sessions, envs, prices, err := load(c)
	if err != nil {
		return err
	}
	m := report.Build(sessions, envs, prices, opt)
	switch c.String("format") {
	case "table":
		return m.WriteText(os.Stdout)
	case "csv":
		return m.WriteCSV(os.Stdout)
	case "json":
		return m.WriteJSON(os.Stdout)
	}
	return fmt.Errorf("format %q: want table, csv or json", c.String("format"))
}

// runCompare splits the sessions at a date and reports what changed across
// it. Without --at the split is the first session whose own tool calls show
// quality-harness, the component whose arrival M calls the QAM installation.
func runCompare(_ context.Context, c *cli.Command) error {
	bucket := c.String("by")
	switch bucket {
	case "month", "week", "day":
	default:
		return fmt.Errorf("by %q: want month, week or day", bucket)
	}
	sessions, envs, prices, err := load(c)
	if err != nil {
		return err
	}
	split, err := parseDate(c.String("at"))
	if err != nil {
		return err
	}
	reason := "given with --at"
	if split.IsZero() {
		first := report.FirstUses(sessions)
		if first.QualityHarness.IsZero() {
			return errors.New("no collected session used quality-harness; pass --at YYYY-MM-DD")
		}
		split = first.QualityHarness.UTC().Truncate(24 * time.Hour)
		reason = "the first session that used quality-harness"
	}
	cmp := report.Compare(sessions, envs, prices, split, reason, bucket)
	switch c.String("format") {
	case "table":
		return cmp.WriteText(os.Stdout)
	case "csv":
		return cmp.WriteCSV(os.Stdout)
	case "json":
		return cmp.WriteJSON(os.Stdout)
	}
	return fmt.Errorf("format %q: want table, csv or json", c.String("format"))
}

func runSessions(_ context.Context, c *cli.Command) error {
	opt, err := options(c)
	if err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.String("limit"))
	if err != nil || limit < 0 {
		return fmt.Errorf("limit %q: want a non-negative integer", c.String("limit"))
	}
	sessions, envs, prices, err := load(c)
	if err != nil {
		return err
	}
	rows := report.SessionRows(sessions, envs, prices, opt)
	if cohort := c.String("cohort"); cohort != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.Cohort == cohort {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	switch c.String("format") {
	case "table":
		return report.WriteSessionsText(os.Stdout, rows)
	case "csv":
		return report.WriteSessionsCSV(os.Stdout, rows)
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	return fmt.Errorf("format %q: want table, csv or json", c.String("format"))
}

// runVerify prices each project's last session the way the collector does
// and sets it beside the figure Claude Code itself wrote into .claude.json,
// with the four token totals it recorded. The transcript holds only the
// requests answered as assistant messages; Claude Code also bills harness
// side calls (the away_summary it generates while the person is away, for
// example) that leave no usage record, so a positive gap is expected and the
// per-field deltas say where it sits.
func runVerify(_ context.Context, c *cli.Command) error {
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(c.String("db"))
	if err != nil {
		return err
	}
	defer st.Close()
	sessions, err := st.Sessions()
	if err != nil {
		return err
	}
	refs, err := st.CostReferences()
	if err != nil {
		return err
	}
	byID := make(map[string]usage.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "project\tsession\tclaude_code_usd\tours_usd\tdelta_pct\tmissing_input\tmissing_output\tmissing_cache_write\tmissing_cache_read\tsubagents")
	var compared, within int
	var worst, sumTheirs, sumOurs float64
	for _, ref := range refs {
		if ref.LastCost <= 0 {
			continue
		}
		s, ok := byID[ref.SessionID]
		if !ok {
			fmt.Fprintf(tw, "%s\t%s\t%.4f\t-\t-\t-\t-\t-\t-\ttranscript not collected\n", filepath.Base(ref.CWD), shortID(ref.SessionID), ref.LastCost)
			continue
		}
		var all usage.Tokens
		var costAll float64
		for _, r := range s.Requests {
			all.Add(r.Tokens)
			costAll += prices.Cost(r.Model, r.Tokens).USD
		}
		pct := (costAll - ref.LastCost) / ref.LastCost * 100
		compared++
		sumTheirs += ref.LastCost
		sumOurs += costAll
		if math.Abs(pct) <= 1 {
			within++
		}
		worst = math.Max(worst, math.Abs(pct))
		fmt.Fprintf(tw, "%s\t%s\t%.4f\t%.4f\t%+.2f%%\t%+d\t%+d\t%+d\t%+d\t%d\n",
			filepath.Base(ref.CWD), shortID(ref.SessionID), ref.LastCost, costAll, pct,
			ref.Input-all.Input, ref.Output-all.Output, ref.CacheWrite-all.CacheWrite, ref.CacheRead-all.CacheRead, s.Subagents)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if compared > 0 {
		fmt.Printf("\n%d references compared: transcript-recorded cost is %.2f%% of Claude Code's own total (%.2f of %.2f USD); %d within 1%%, worst delta %.2f%%\n",
			compared, sumOurs/sumTheirs*100, sumOurs, sumTheirs, within, worst)
	}
	fmt.Println("missing_* is Claude Code's recorded total minus the transcript's: requests the harness made without writing an assistant message (session summaries and the like).")
	fmt.Println("The matrix reports transcript-recorded usage only, so every cohort is a lower bound by the same mechanism.")
	return nil
}

func runPrices(_ context.Context, _ *cli.Command) error {
	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	fmt.Printf("source: %s\nas of: %s\n\n", prices.Source, prices.AsOf)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "model\tinput\toutput\tcache_read\tcache_write_5m\tcache_write_1h\tverified\tnote")
	for _, id := range prices.ModelIDs() {
		p := prices.Models[id]
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%.3f\t%.3f\t%.2f\t%t\t%s\n", id, p.Input, p.Output, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Verified, p.Note)
	}
	return tw.Flush()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
