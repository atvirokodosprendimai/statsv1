// Package store persists collected sessions, requests, environments and cost
// references in one SQLite file. Collecting is the single write path; every
// report reads. Re-collecting the same transcripts is idempotent, because
// every row is keyed by the identity Claude Code already gave it.
package store

import (
	"embed"
	"fmt"
	"sort"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pressly/goose/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is an open SQLite database.
type Store struct {
	db *gorm.DB
}

// Open opens (creating if needed) the database at path and applies the
// embedded goose migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return nil, err
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

type configDirRow struct {
	Path              string `gorm:"primaryKey;column:path"`
	Label             string `gorm:"column:label"`
	HasAgentsmemory   bool   `gorm:"column:has_agentsmemory"`
	HasQualityHarness bool   `gorm:"column:has_quality_harness"`
	QHInstalledAt     string `gorm:"column:qh_installed_at"`
	ScannedAt         string `gorm:"column:scanned_at"`
}

func (configDirRow) TableName() string { return "config_dirs" }

type sessionRow struct {
	SessionID      string `gorm:"primaryKey;column:session_id"`
	ConfigDir      string `gorm:"column:config_dir"`
	ProjectSlug    string `gorm:"column:project_slug"`
	CWD            string `gorm:"column:cwd"`
	TranscriptPath string `gorm:"column:transcript_path"`
	ClaudeVersion  string `gorm:"column:claude_version"`
	StartedAt      string `gorm:"column:started_at"`
	EndedAt        string `gorm:"column:ended_at"`
	UserTurns      int    `gorm:"column:user_turns"`
	ToolCalls      int    `gorm:"column:tool_calls"`
	Subagents      int    `gorm:"column:subagents"`
	AMCalls        int    `gorm:"column:am_calls"`
	MRWCalls       int    `gorm:"column:mrw_calls"`
	QHCalls        int    `gorm:"column:qh_calls"`
	Cohort         string `gorm:"column:cohort"`
}

func (sessionRow) TableName() string { return "sessions" }

type requestRow struct {
	RequestKey         string `gorm:"primaryKey;column:request_key"`
	SessionID          string `gorm:"column:session_id"`
	MessageID          string `gorm:"column:message_id"`
	Iteration          int    `gorm:"column:iteration"`
	RequestID          string `gorm:"column:request_id"`
	At                 string `gorm:"column:at"`
	Model              string `gorm:"column:model"`
	InputTokens        int64  `gorm:"column:input_tokens"`
	OutputTokens       int64  `gorm:"column:output_tokens"`
	CacheReadTokens    int64  `gorm:"column:cache_read_tokens"`
	CacheWriteTokens   int64  `gorm:"column:cache_write_tokens"`
	CacheWrite5mTokens int64  `gorm:"column:cache_write_5m_tokens"`
	CacheWrite1hTokens int64  `gorm:"column:cache_write_1h_tokens"`
	TTLKnown           bool   `gorm:"column:ttl_known"`
	IsSubagent         bool   `gorm:"column:is_subagent"`
	AgentID            string `gorm:"column:agent_id"`
	ToolUses           int    `gorm:"column:tool_uses"`
}

func (requestRow) TableName() string { return "requests" }

type costReferenceRow struct {
	ConfigDir        string  `gorm:"primaryKey;column:config_dir"`
	CWD              string  `gorm:"primaryKey;column:cwd"`
	SessionID        string  `gorm:"column:session_id"`
	LastCostUSD      float64 `gorm:"column:last_cost_usd"`
	InputTokens      int64   `gorm:"column:input_tokens"`
	OutputTokens     int64   `gorm:"column:output_tokens"`
	CacheWriteTokens int64   `gorm:"column:cache_write_tokens"`
	CacheReadTokens  int64   `gorm:"column:cache_read_tokens"`
	DurationMS       int64   `gorm:"column:duration_ms"`
}

func (costReferenceRow) TableName() string { return "cost_references" }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

var upsert = clause.OnConflict{UpdateAll: true}

// PutEnvironment records what a config directory had installed when scanned.
func (s *Store) PutEnvironment(e usage.Environment) error {
	row := configDirRow{Path: e.Dir, Label: e.Label(), HasAgentsmemory: e.HasAgentsmemory, HasQualityHarness: e.HasQualityHarness,
		QHInstalledAt: formatTime(e.QualityHarnessInstalledAt), ScannedAt: formatTime(e.ScannedAt)}
	return s.db.Clauses(upsert).Create(&row).Error
}

// PutSession writes a session and its requests, replacing what an earlier
// collect stored for the same identities.
func (s *Store) PutSession(sess usage.Session) error {
	row := sessionRow{
		SessionID: sess.ID, ConfigDir: sess.ConfigDir, ProjectSlug: sess.ProjectSlug, CWD: sess.CWD,
		TranscriptPath: sess.TranscriptPath, ClaudeVersion: sess.Version,
		StartedAt: formatTime(sess.StartedAt), EndedAt: formatTime(sess.EndedAt),
		UserTurns: sess.UserTurns, ToolCalls: sess.ToolCalls, Subagents: sess.Subagents,
		AMCalls: sess.Signals.AM, MRWCalls: sess.Signals.MRW, QHCalls: sess.Signals.QH, Cohort: sess.Signals.Cohort(),
	}
	rows := make([]requestRow, 0, len(sess.Requests))
	for _, r := range sess.Requests {
		rows = append(rows, requestRow{
			RequestKey: fmt.Sprintf("%s|%s#%d", sess.ID, r.MessageID, r.Iteration), SessionID: sess.ID, MessageID: r.MessageID, Iteration: r.Iteration, RequestID: r.RequestID,
			At: formatTime(r.At), Model: r.Model,
			InputTokens: r.Tokens.Input, OutputTokens: r.Tokens.Output, CacheReadTokens: r.Tokens.CacheRead,
			CacheWriteTokens: r.Tokens.CacheWrite, CacheWrite5mTokens: r.Tokens.CacheWrite5m, CacheWrite1hTokens: r.Tokens.CacheWrite1h,
			TTLKnown: r.Tokens.TTLKnown, IsSubagent: r.IsSubagent, AgentID: r.AgentID, ToolUses: r.ToolUses,
		})
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(upsert).Create(&row).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Clauses(upsert).CreateInBatches(rows, 500).Error
	})
}

// PutCostReferences records Claude Code's own last-session costs.
func (s *Store) PutCostReferences(refs []usage.CostReference) error {
	if len(refs) == 0 {
		return nil
	}
	rows := make([]costReferenceRow, 0, len(refs))
	for _, r := range refs {
		rows = append(rows, costReferenceRow{ConfigDir: r.ConfigDir, CWD: r.CWD, SessionID: r.SessionID, LastCostUSD: r.LastCost,
			InputTokens: r.Input, OutputTokens: r.Output, CacheWriteTokens: r.CacheWrite, CacheReadTokens: r.CacheRead, DurationMS: r.DurationMS})
	}
	return s.db.Clauses(upsert).CreateInBatches(rows, 500).Error
}

// Environments returns every scanned config directory, keyed by path.
func (s *Store) Environments() (map[string]usage.Environment, error) {
	var rows []configDirRow
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]usage.Environment, len(rows))
	for _, r := range rows {
		out[r.Path] = usage.Environment{Dir: r.Path, HasAgentsmemory: r.HasAgentsmemory, HasQualityHarness: r.HasQualityHarness,
			QualityHarnessInstalledAt: parseTime(r.QHInstalledAt), ScannedAt: parseTime(r.ScannedAt)}
	}
	return out, nil
}

// Sessions returns every stored session with its requests, oldest first.
func (s *Store) Sessions() ([]usage.Session, error) {
	var srows []sessionRow
	if err := s.db.Order("started_at").Find(&srows).Error; err != nil {
		return nil, err
	}
	var rrows []requestRow
	if err := s.db.Order("at, iteration").Find(&rrows).Error; err != nil {
		return nil, err
	}
	byID := make(map[string][]usage.Request, len(srows))
	for _, r := range rrows {
		byID[r.SessionID] = append(byID[r.SessionID], usage.Request{
			SessionID: r.SessionID, MessageID: r.MessageID, Iteration: r.Iteration, RequestID: r.RequestID, At: parseTime(r.At), Model: r.Model,
			Tokens: usage.Tokens{Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens, CacheWrite: r.CacheWriteTokens,
				CacheWrite5m: r.CacheWrite5mTokens, CacheWrite1h: r.CacheWrite1hTokens, TTLKnown: r.TTLKnown},
			IsSubagent: r.IsSubagent, AgentID: r.AgentID, ToolUses: r.ToolUses,
		})
	}
	out := make([]usage.Session, 0, len(srows))
	for _, r := range srows {
		out = append(out, usage.Session{
			ID: r.SessionID, ConfigDir: r.ConfigDir, ProjectSlug: r.ProjectSlug, CWD: r.CWD, TranscriptPath: r.TranscriptPath, Version: r.ClaudeVersion,
			StartedAt: parseTime(r.StartedAt), EndedAt: parseTime(r.EndedAt), UserTurns: r.UserTurns, ToolCalls: r.ToolCalls, Subagents: r.Subagents,
			Signals: usage.Signals{AM: r.AMCalls, MRW: r.MRWCalls, QH: r.QHCalls}, Requests: byID[r.SessionID],
		})
	}
	return out, nil
}

// CostReferences returns Claude Code's own last-session costs, by config dir
// and working directory.
func (s *Store) CostReferences() ([]usage.CostReference, error) {
	var rows []costReferenceRow
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]usage.CostReference, 0, len(rows))
	for _, r := range rows {
		out = append(out, usage.CostReference{ConfigDir: r.ConfigDir, CWD: r.CWD, SessionID: r.SessionID, LastCost: r.LastCostUSD,
			Input: r.InputTokens, Output: r.OutputTokens, CacheWrite: r.CacheWriteTokens, CacheRead: r.CacheReadTokens, DurationMS: r.DurationMS})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConfigDir != out[j].ConfigDir {
			return out[i].ConfigDir < out[j].ConfigDir
		}
		return out[i].CWD < out[j].CWD
	})
	return out, nil
}
