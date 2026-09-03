// Package claudecode reads Claude Code's on-disk state: the JSONL transcripts
// under <config>/projects (main sessions and their subagents), the installed
// MCP servers and plugins that describe a config directory, and the
// per-project cost fields Claude Code itself writes into .claude.json.
package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

// maxLine bounds one transcript line. Tool results are stored inline, so a
// single line can be tens of megabytes.
const maxLine = 256 << 20

// DiscoverConfigDirs returns every Claude Code config directory on this
// machine that holds transcripts: $CLAUDE_CONFIG_DIR, ~/.claude, and each
// sandbox under ~/.sandboxes. A directory without a projects/ folder is not a
// config directory for this purpose.
func DiscoverConfigDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" {
			return
		}
		d = filepath.Clean(d)
		if seen[d] {
			return
		}
		if st, err := os.Stat(filepath.Join(d, "projects")); err != nil || !st.IsDir() {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	add(os.Getenv("CLAUDE_CONFIG_DIR"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude"))
		matches, _ := filepath.Glob(filepath.Join(home, ".sandboxes", "*"))
		sort.Strings(matches)
		for _, m := range matches {
			add(m)
		}
	}
	return dirs
}

// ScanResult is what one config directory yielded.
type ScanResult struct {
	Sessions     []usage.Session
	Files        int
	SkippedLines int
}

// ScanProjects reads every transcript under <configDir>/projects into
// sessions. A session's subagent transcripts, stored beside the main file as
// <session>/subagents/agent-*.jsonl, are folded into the same session.
func ScanProjects(configDir string) (ScanResult, error) {
	root := filepath.Join(configDir, "projects")
	slugs, err := os.ReadDir(root)
	if err != nil {
		return ScanResult{}, err
	}
	var res ScanResult
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		slugDir := filepath.Join(root, slug.Name())
		entries, err := os.ReadDir(slugDir)
		if err != nil {
			return res, err
		}
		sessions := map[string]*usage.Session{}
		var order []string
		get := func(id string) *usage.Session {
			if s, ok := sessions[id]; ok {
				return s
			}
			s := &usage.Session{ID: id, ConfigDir: configDir, ProjectSlug: slug.Name()}
			sessions[id] = s
			order = append(order, id)
			return s
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case !e.IsDir() && strings.HasSuffix(name, ".jsonl"):
				s := get(strings.TrimSuffix(name, ".jsonl"))
				s.TranscriptPath = filepath.Join(slugDir, name)
				skipped, err := parseInto(s, s.TranscriptPath, "")
				if err != nil {
					return res, fmt.Errorf("%s: %w", s.TranscriptPath, err)
				}
				res.Files++
				res.SkippedLines += skipped
			case e.IsDir():
				agents, _ := filepath.Glob(filepath.Join(slugDir, name, "subagents", "agent-*.jsonl"))
				if len(agents) == 0 {
					continue
				}
				sort.Strings(agents)
				s := get(name)
				for _, path := range agents {
					agentID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
					skipped, err := parseInto(s, path, agentID)
					if err != nil {
						return res, fmt.Errorf("%s: %w", path, err)
					}
					res.Files++
					res.SkippedLines += skipped
					s.Subagents++
				}
			}
		}
		for _, id := range order {
			s := sessions[id]
			sort.SliceStable(s.Requests, func(i, j int) bool { return s.Requests[i].At.Before(s.Requests[j].At) })
			res.Sessions = append(res.Sessions, *s)
		}
	}
	return res, nil
}

// record is one transcript line. Only the fields the collector reads are
// declared; the message content stays raw until its type says it is needed.
type record struct {
	Type             string      `json:"type"`
	UUID             string      `json:"uuid"`
	Timestamp        string      `json:"timestamp"`
	CWD              string      `json:"cwd"`
	Version          string      `json:"version"`
	IsMeta           bool        `json:"isMeta"`
	IsCompactSummary bool        `json:"isCompactSummary"`
	RequestID        string      `json:"requestId"`
	Message          *message    `json:"message"`
	Attachment       *attachment `json:"attachment"`
}

// attachment is the payload of an "attachment" record. A prompt the person
// typed while the model was still working is delivered as a queued_command
// attachment rather than as a user message; commandMode separates a typed
// prompt from a harness task notification carried the same way, and origin
// names who queued it where the harness recorded that.
type attachment struct {
	Type        string          `json:"type"`
	CommandMode string          `json:"commandMode"`
	Origin      json.RawMessage `json:"origin"`
}

type message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Usage   *apiUsage       `json:"usage"`
	Content json.RawMessage `json:"content"`
}

// usageFields are the billed counters of one attempt.
type usageFields struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *cacheCreation `json:"cache_creation"`
}

type cacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

// apiUsage is the usage block of an assistant message. Its top-level counters
// describe the LAST attempt only; when the API fell back to another model
// after a refusal, iterations lists every billed attempt with its own model,
// and the collector prices each of them.
type apiUsage struct {
	usageFields
	Iterations []iterationUsage `json:"iterations"`
}

type iterationUsage struct {
	usageFields
	Type  string `json:"type"`
	Model string `json:"model"`
}

type block struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Text  string          `json:"text"`
}

// toolInput carries the three input fields that reveal QAM use: the shell
// command of Bash, the skill name of Skill, the subagent type of Agent.
type toolInput struct {
	Command      string `json:"command"`
	Skill        string `json:"skill"`
	SubagentType string `json:"subagent_type"`
}

// parseInto folds one transcript file into the session. agentID is empty for
// the main transcript and the subagent id for a subagent transcript. It
// returns how many lines did not decode as JSON.
func parseInto(s *usage.Session, path, agentID string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// byMsg maps a message id to the indexes of its attempts in s.Requests,
	// so a message repeated once per content block is counted once.
	byMsg := make(map[string][]int, len(s.Requests))
	for i, r := range s.Requests {
		byMsg[r.MessageID] = append(byMsg[r.MessageID], i)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), maxLine)
	skipped := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			skipped++
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if !at.IsZero() {
			if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
				s.StartedAt = at
			}
			if at.After(s.EndedAt) {
				s.EndedAt = at
			}
		}
		if agentID == "" {
			if s.CWD == "" {
				s.CWD = rec.CWD
			}
			if s.Version == "" {
				s.Version = rec.Version
			}
		}
		if rec.Type == "attachment" && rec.Attachment != nil {
			// A queued prompt reached the model exactly like a typed one; only
			// the auto-continuation the harness queues for itself is not a turn.
			if agentID == "" && rec.Attachment.Type == "queued_command" && rec.Attachment.CommandMode == "prompt" &&
				!bytes.Contains(rec.Attachment.Origin, []byte("auto-continuation")) {
				s.UserTurns++
			}
			continue
		}
		if rec.Message == nil {
			continue
		}
		switch rec.Type {
		case "assistant":
			toolUses := 0
			for _, b := range decodeBlocks(rec.Message.Content) {
				if b.Type != "tool_use" {
					continue
				}
				toolUses++
				s.ToolCalls++
				var in toolInput
				_ = json.Unmarshal(b.Input, &in)
				s.Signals.Add(usage.Classify(b.Name, in.Command, in.Skill, in.SubagentType))
			}
			u := rec.Message.Usage
			if u == nil || rec.Message.Model == "" || rec.Message.Model == "<synthetic>" {
				continue
			}
			key := rec.Message.ID
			if key == "" {
				key = rec.RequestID
			}
			if key == "" {
				key = rec.UUID
			}
			attempts := attemptsOf(u, rec.Message.Model)
			if rows, ok := byMsg[key]; ok {
				for i, idx := range rows {
					if i < len(attempts) {
						s.Requests[idx].Tokens.Max(attempts[i].tokens)
					}
				}
				s.Requests[rows[len(rows)-1]].ToolUses += toolUses
				continue
			}
			for i, a := range attempts {
				byMsg[key] = append(byMsg[key], len(s.Requests))
				req := usage.Request{
					SessionID:  s.ID,
					MessageID:  key,
					Iteration:  i,
					RequestID:  rec.RequestID,
					At:         at,
					Model:      a.model,
					Tokens:     a.tokens,
					IsSubagent: agentID != "",
					AgentID:    agentID,
				}
				if i == len(attempts)-1 {
					req.ToolUses = toolUses
				}
				s.Requests = append(s.Requests, req)
			}
		case "user":
			// A subagent's first user message is the dispatcher's brief, a meta
			// record is context the harness injected, and a compaction summary
			// is the harness talking to the model; none is a human turn.
			if agentID != "" || rec.IsMeta || rec.IsCompactSummary {
				continue
			}
			if isHumanTurn(rec.Message.Content) {
				s.UserTurns++
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return skipped, fmt.Errorf("a line exceeds %d bytes", maxLine)
		}
		return skipped, err
	}
	return skipped, nil
}

// attempt is one billed attempt of a message with the model that served it.
type attempt struct {
	model  string
	tokens usage.Tokens
}

// attemptsOf lists every billed attempt of a message. Without an iterations
// list the top-level counters are the only attempt; with one, each iteration
// is an attempt under its own model, falling back to the message's model when
// an iteration does not name one.
func attemptsOf(u *apiUsage, model string) []attempt {
	if len(u.Iterations) == 0 {
		return []attempt{{model: model, tokens: tokensOf(&u.usageFields)}}
	}
	out := make([]attempt, 0, len(u.Iterations))
	for _, it := range u.Iterations {
		m := it.Model
		if m == "" {
			m = model
		}
		out = append(out, attempt{model: m, tokens: tokensOf(&it.usageFields)})
	}
	return out
}

func tokensOf(u *usageFields) usage.Tokens {
	t := usage.Tokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	// The breakdown is trusted only when it accounts for every cache-write
	// token; otherwise the request is priced under the 5-minute assumption
	// and reported as such.
	if cc := u.CacheCreation; cc != nil && cc.Ephemeral5m+cc.Ephemeral1h == u.CacheCreationInputTokens {
		t.TTLKnown = true
		t.CacheWrite5m = cc.Ephemeral5m
		t.CacheWrite1h = cc.Ephemeral1h
	}
	return t
}

func decodeBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// harnessPrefixes open user records the harness wrote on the person's behalf:
// built-in slash commands (which put <command-name> first, where a skill
// invocation puts <command-message> first), local command echoes, background
// task notifications, interrupts and hook feedback. Measured over every
// transcript on this machine on 2026-09-03.
var harnessPrefixes = []string{
	"<command-name>", "<local-command-", "<task-notification>", "<task-id>", "[Request interrupted", "Stop hook feedback:",
}

var systemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// isHumanTurn reports whether a user record is a prompt the person sent to
// the model: typed text, a pasted image, or a skill invocation. Tool results,
// harness-injected records and built-in slash commands are not turns.
func isHumanTurn(raw json.RawMessage) bool {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		var parts []string
		for _, b := range decodeBlocks(raw) {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "<command-message>") {
		return true
	}
	for _, p := range harnessPrefixes {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return strings.TrimSpace(systemReminder.ReplaceAllString(t, "")) != ""
}

// claudeJSON is the slice of .claude.json the collector reads: the user-scope
// MCP registrations and the per-project last-session cost fields.
type claudeJSON struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		LastCost                          *float64 `json:"lastCost"`
		LastSessionID                     string   `json:"lastSessionId"`
		LastTotalInputTokens              int64    `json:"lastTotalInputTokens"`
		LastTotalOutputTokens             int64    `json:"lastTotalOutputTokens"`
		LastTotalCacheCreationInputTokens int64    `json:"lastTotalCacheCreationInputTokens"`
		LastTotalCacheReadInputTokens     int64    `json:"lastTotalCacheReadInputTokens"`
		LastDuration                      int64    `json:"lastDuration"`
	} `json:"projects"`
}

func readClaudeJSON(configDir string) (claudeJSON, error) {
	var cj claudeJSON
	data, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		return cj, err
	}
	if err := json.Unmarshal(data, &cj); err != nil {
		return cj, fmt.Errorf(".claude.json: %w", err)
	}
	return cj, nil
}

// ReadCostReferences returns Claude Code's own cost record for each project's
// last session, from .claude.json. Projects that never recorded a cost are
// omitted. A missing .claude.json yields no references and no error.
func ReadCostReferences(configDir string) ([]usage.CostReference, error) {
	cj, err := readClaudeJSON(configDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []usage.CostReference
	for cwd, p := range cj.Projects {
		if p.LastCost == nil || p.LastSessionID == "" {
			continue
		}
		refs = append(refs, usage.CostReference{
			ConfigDir:  configDir,
			CWD:        cwd,
			SessionID:  p.LastSessionID,
			LastCost:   *p.LastCost,
			Input:      p.LastTotalInputTokens,
			Output:     p.LastTotalOutputTokens,
			CacheWrite: p.LastTotalCacheCreationInputTokens,
			CacheRead:  p.LastTotalCacheReadInputTokens,
			DurationMS: p.LastDuration,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].CWD < refs[j].CWD })
	return refs, nil
}

// ReadEnvironment reports which QAM components a config directory has
// installed: agentsmemory as a user-scope MCP server in .claude.json, and the
// quality-harness plugin in plugins/installed_plugins.json, with the earliest
// installedAt the registry records for it.
func ReadEnvironment(configDir string, now time.Time) usage.Environment {
	env := usage.Environment{Dir: configDir, ScannedAt: now}
	if cj, err := readClaudeJSON(configDir); err == nil {
		_, env.HasAgentsmemory = cj.MCPServers["agentsmemory"]
	}
	if data, err := os.ReadFile(filepath.Join(configDir, "plugins", "installed_plugins.json")); err == nil {
		var top map[string]json.RawMessage
		if json.Unmarshal(data, &top) == nil {
			keys := top
			if nested, ok := top["plugins"]; ok {
				var inner map[string]json.RawMessage
				if json.Unmarshal(nested, &inner) == nil {
					keys = inner
				}
			}
			for k, raw := range keys {
				if !strings.HasPrefix(k, "quality-harness") {
					continue
				}
				env.HasQualityHarness = true
				if at := earliestInstall(raw); !at.IsZero() && (env.QualityHarnessInstalledAt.IsZero() || at.Before(env.QualityHarnessInstalledAt)) {
					env.QualityHarnessInstalledAt = at
				}
			}
		}
	}
	return env
}

// earliestInstall reads the installedAt of every entry a plugin lists in the
// registry (one per scope) and returns the earliest; zero when none parses.
func earliestInstall(raw json.RawMessage) time.Time {
	var entries []struct {
		InstalledAt string `json:"installedAt"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, e := range entries {
		at, err := time.Parse(time.RFC3339Nano, e.InstalledAt)
		if err != nil {
			continue
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest
}
