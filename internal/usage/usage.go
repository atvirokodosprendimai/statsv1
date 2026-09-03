// Package usage is the domain of the collector: one API request with its token
// usage, the session that owns it, and the rules that label a session by the
// QAM components (Quality Harness, AI Agent Memory, MultiPath Read/Write) its
// own tool calls show it used.
package usage

import (
	"regexp"
	"strings"
	"time"
)

// Tokens is the token usage of one API request, as Claude Code records it in
// the assistant message's usage block.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64 // cache_creation_input_tokens, whatever the TTL
	// CacheWrite5m and CacheWrite1h split CacheWrite by TTL. They are only
	// meaningful when TTLKnown is true; older transcripts carry no breakdown.
	CacheWrite5m int64
	CacheWrite1h int64
	TTLKnown     bool
}

// Add accumulates o into t. TTLKnown is left alone: it describes a single
// request, and an aggregate reports assumed tokens separately.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
}

// Max keeps the larger of each counter. A transcript repeats one message once
// per content block with the same usage; keeping the maximum is robust to a
// record written before the final output count was known.
func (t *Tokens) Max(o Tokens) {
	t.Input = max(t.Input, o.Input)
	t.Output = max(t.Output, o.Output)
	t.CacheRead = max(t.CacheRead, o.CacheRead)
	t.CacheWrite = max(t.CacheWrite, o.CacheWrite)
	t.CacheWrite5m = max(t.CacheWrite5m, o.CacheWrite5m)
	t.CacheWrite1h = max(t.CacheWrite1h, o.CacheWrite1h)
	t.TTLKnown = t.TTLKnown || o.TTLKnown
}

// Request is one billed attempt of a deduplicated API call made by a session,
// main transcript or subagent transcript alike. A message normally has one
// attempt; a server-side fallback bills two, the refused attempt and the
// fallback that answered, each under its own model.
type Request struct {
	SessionID  string
	MessageID  string
	Iteration  int
	RequestID  string
	At         time.Time
	Model      string
	Tokens     Tokens
	IsSubagent bool
	AgentID    string
	ToolUses   int
}

// Signals counts the tool calls that reveal each QAM component in use.
type Signals struct {
	AM  int // agentsmemory MCP tool calls: the A of QAM
	MRW int // mrw invocations from the shell: the M of QAM
	QH  int // quality-harness skills, agents and gate binaries: the Q of QAM
}

// Add accumulates o into s.
func (s *Signals) Add(o Signals) {
	s.AM += o.AM
	s.MRW += o.MRW
	s.QH += o.QH
}

// Cohort names the QAM components a session actually used: "QAM" when all
// three appear, "none" when none does, otherwise "partial:" followed by the
// letters present in the Q, A, M order the stack itself uses.
func (s Signals) Cohort() string {
	var b strings.Builder
	if s.QH > 0 {
		b.WriteString("Q")
	}
	if s.AM > 0 {
		b.WriteString("A")
	}
	if s.MRW > 0 {
		b.WriteString("M")
	}
	switch b.Len() {
	case 0:
		return "none"
	case 3:
		return "QAM"
	}
	return "partial:" + b.String()
}

// Session is the aggregate: one Claude Code conversation, its main transcript
// plus every subagent transcript spawned from it, with the deduplicated
// requests and the tool-call signals that label it.
type Session struct {
	ID             string
	ConfigDir      string
	ProjectSlug    string
	CWD            string
	TranscriptPath string
	Version        string
	StartedAt      time.Time
	EndedAt        time.Time
	UserTurns      int
	ToolCalls      int
	Subagents      int
	Signals        Signals
	Requests       []Request
}

// Environment is what a Claude Code config directory has installed when it is
// scanned: the agentsmemory MCP registration and the quality-harness plugin.
// mrw is a binary on PATH and leaves no trace in a config directory, so the
// environment can only witness A and Q.
type Environment struct {
	Dir               string
	HasAgentsmemory   bool
	HasQualityHarness bool
	ScannedAt         time.Time
}

// Label names the environment for the report.
func (e Environment) Label() string {
	switch {
	case e.HasAgentsmemory && e.HasQualityHarness:
		return "qam-installed"
	case e.HasAgentsmemory:
		return "agentsmemory-only"
	case e.HasQualityHarness:
		return "quality-harness-only"
	}
	return "plain"
}

// CostReference is Claude Code's own record of what a project's last session
// cost, read from the per-project fields of .claude.json. It is the oracle the
// verify command compares the collector's arithmetic against.
type CostReference struct {
	ConfigDir  string
	CWD        string
	SessionID  string
	LastCost   float64
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
	DurationMS int64
}

// The detection rules. A shell command counts as an mrw use only when it runs
// one of mrw's subcommands, so building mrw inside its own repository is not
// mistaken for using it. A gate binary counts wherever it is invoked.
var (
	mrwCommand  = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(\S*/)?mrw\s+(-C|--root|read|write|check|iter|seen|--version|version)\b`)
	gateCommand = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(\S*/)?(adr-lint|adr-verify|adr-judge|adr-next|adr-debt|adr-retire-check|arch-lint|postmortem-verify|qh-mcp|qh-root)([^A-Za-z0-9_-]|$)`)
)

// Classify reports which QAM component one tool_use block reveals. name is the
// tool name; command, skill and subagentType are the input fields that carry
// the evidence for Bash, Skill and Agent respectively. One Bash command can
// count as both M and Q when it runs mrw and a gate.
func Classify(name, command, skill, subagentType string) Signals {
	var s Signals
	switch {
	case strings.HasPrefix(name, "mcp__agentsmemory__"):
		s.AM++
	case name == "Skill" && strings.HasPrefix(skill, "quality-harness:"):
		s.QH++
	case (name == "Agent" || name == "Task") && strings.HasPrefix(subagentType, "quality-harness:"):
		s.QH++
	case name == "Bash":
		if mrwCommand.MatchString(command) {
			s.MRW++
		}
		if gateCommand.MatchString(command) {
			s.QH++
		}
	}
	return s
}
