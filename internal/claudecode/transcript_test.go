package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture mirrors the shapes measured on real transcripts on 2026-09-03:
// an assistant message repeated once per content block with identical usage;
// human turns as a string, a text block and two skill invocations; the
// records the harness writes as the user (a tool result, meta context, a
// local command echo, a built-in slash command, a compaction summary, a task
// notification, an interrupt); a synthetic assistant record without a model
// price; a fallback message billed as two iterations, repeated; and a
// subagent transcript stored beside the main file.
const mainTranscript = `{"type":"user","uuid":"u1","timestamp":"2026-09-01T10:00:00.000Z","sessionId":"s1","cwd":"/w/proj","version":"2.1.236","message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-09-01T10:00:01.000Z","sessionId":"s1","cwd":"/w/proj","version":"2.1.236","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":2,"output_tokens":90,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100}}}}
{"type":"assistant","uuid":"a2","timestamp":"2026-09-01T10:00:01.500Z","sessionId":"s1","cwd":"/w/proj","version":"2.1.236","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"mrw read a.go"}}],"usage":{"input_tokens":2,"output_tokens":90,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100}}}}
{"type":"user","uuid":"u2","timestamp":"2026-09-01T10:00:02.000Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"user","uuid":"u3","isMeta":true,"timestamp":"2026-09-01T10:00:02.100Z","sessionId":"s1","message":{"role":"user","content":"injected context"}}
{"type":"user","uuid":"u4","timestamp":"2026-09-01T10:00:02.200Z","sessionId":"s1","message":{"role":"user","content":"<local-command-stdout>ignored</local-command-stdout>"}}
{"type":"assistant","uuid":"a3","timestamp":"2026-09-01T10:00:03.000Z","sessionId":"s1","requestId":"req_2","message":{"id":"msg_2","role":"assistant","model":"claude-fable-5-1","content":[{"type":"tool_use","id":"t2","name":"mcp__agentsmemory__am_search","input":{"query":"x"}}],"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":30,"cache_read_input_tokens":200}}}
{"type":"assistant","uuid":"a4","timestamp":"2026-09-01T10:00:04.000Z","sessionId":"s1","message":{"id":"msg_3","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"synthetic"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"attachment","uuid":"x1","timestamp":"2026-09-01T10:00:05.000Z","sessionId":"s1"}
not json at all
{"type":"user","uuid":"u5","timestamp":"2026-09-01T10:00:06.000Z","sessionId":"s1","message":{"role":"user","content":[{"type":"text","text":"second question"}]}}
{"type":"user","uuid":"u6","isCompactSummary":true,"timestamp":"2026-09-01T10:00:06.100Z","sessionId":"s1","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context."}}
{"type":"user","uuid":"u7","timestamp":"2026-09-01T10:00:06.200Z","sessionId":"s1","message":{"role":"user","content":"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>"}}
{"type":"user","uuid":"u8","timestamp":"2026-09-01T10:00:06.300Z","sessionId":"s1","message":{"role":"user","content":"<command-message>am</command-message>\n<command-name>/am</command-name>\n<command-args>do the thing</command-args>"}}
{"type":"user","uuid":"u9","timestamp":"2026-09-01T10:00:06.400Z","sessionId":"s1","message":{"role":"user","content":"<task-notification>\n<task-id>b3q97ca5q</task-id>\n<status>completed</status>\n</task-notification>"}}
{"type":"user","uuid":"u10","timestamp":"2026-09-01T10:00:06.500Z","sessionId":"s1","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
{"type":"assistant","uuid":"a5","timestamp":"2026-09-01T10:00:07.000Z","sessionId":"s1","requestId":"req_4","message":{"id":"msg_4","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"fallback"}],"usage":{"input_tokens":2,"output_tokens":20,"cache_creation_input_tokens":6,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":6},"iterations":[{"type":"message","model":"claude-fable-5","input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5}},{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":2,"output_tokens":20,"cache_creation_input_tokens":6,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":6}}]}}}
{"type":"assistant","uuid":"a6","timestamp":"2026-09-01T10:00:07.500Z","sessionId":"s1","requestId":"req_4","message":{"id":"msg_4","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"fallback continued"}],"usage":{"input_tokens":2,"output_tokens":20,"cache_creation_input_tokens":6,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":6},"iterations":[{"type":"message","model":"claude-fable-5","input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5}},{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":2,"output_tokens":20,"cache_creation_input_tokens":6,"cache_read_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":6}}]}}}
{"type":"user","uuid":"u11","timestamp":"2026-09-01T10:00:08.000Z","sessionId":"s1","message":{"role":"user","content":"<command-message>M</command-message>\n<command-name>/M</command-name>\n<command-args></command-args>"}}
{"type":"attachment","uuid":"x2","timestamp":"2026-09-01T10:00:08.500Z","sessionId":"s1","attachment":{"type":"queued_command","prompt":"and also this","commandMode":"prompt","origin":{"kind":"human"}}}
{"type":"attachment","uuid":"x3","timestamp":"2026-09-01T10:00:08.600Z","sessionId":"s1","attachment":{"type":"queued_command","prompt":"<task-notification>done</task-notification>","commandMode":"task-notification","origin":{"kind":"unclassified"}}}
{"type":"attachment","uuid":"x4","timestamp":"2026-09-01T10:00:08.700Z","sessionId":"s1","attachment":{"type":"queued_command","prompt":"continue","commandMode":"prompt","origin":{"kind":"auto-continuation"}}}
`

const subagentTranscript = `{"type":"user","uuid":"su1","timestamp":"2026-09-01T10:00:03.200Z","sessionId":"s1","message":{"role":"user","content":"dispatcher brief"}}
{"type":"assistant","uuid":"sa1","timestamp":"2026-09-01T10:00:03.500Z","sessionId":"s1","requestId":"req_3","message":{"id":"msg_9","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"t9","name":"Skill","input":{"skill":"quality-harness:adr-write"}}],"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":0}}}}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	slug := filepath.Join(cfg, "projects", "-w-proj")
	if err := os.MkdirAll(filepath.Join(slug, "s1", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug, "s1.jsonl"), []byte(mainTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug, "s1", "subagents", "agent-abc.jsonl"), []byte(subagentTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(slug, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestScanProjectsFoldsMainAndSubagentTranscriptsIntoOneSession(t *testing.T) {
	cfg := writeFixture(t)
	res, err := ScanProjects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.SkippedLines != 1 {
		t.Fatalf("files=%d skipped=%d, want 2 files and 1 skipped line", res.Files, res.SkippedLines)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(res.Sessions))
	}
	s := res.Sessions[0]
	if s.ID != "s1" || s.CWD != "/w/proj" || s.Version != "2.1.236" || s.ProjectSlug != "-w-proj" || s.ConfigDir != cfg {
		t.Errorf("session identity wrong: %+v", s)
	}
	if s.UserTurns != 5 {
		t.Errorf("UserTurns = %d, want 5 (two typed prompts, two skill invocations and one queued prompt; meta, tool_result, local-command echo, built-in command, compaction summary, task notification, interrupt, the auto-continuation and the subagent brief excluded)", s.UserTurns)
	}
	if s.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", s.ToolCalls)
	}
	if s.Signals.AM != 1 || s.Signals.MRW != 1 || s.Signals.QH != 1 || s.Signals.Cohort() != "QAM" {
		t.Errorf("signals = %+v, want one of each and cohort QAM", s.Signals)
	}
	if s.Subagents != 1 {
		t.Errorf("Subagents = %d, want 1", s.Subagents)
	}
	if len(s.Requests) != 5 {
		t.Fatalf("requests = %d, want 5 (msg_1 deduplicated, synthetic dropped, the fallback message billed as two attempts once)", len(s.Requests))
	}
	first := s.Requests[0]
	if first.MessageID != "msg_1" || first.Iteration != 0 || first.ToolUses != 1 || first.Tokens.Output != 90 || first.Tokens.CacheWrite1h != 100 || !first.Tokens.TTLKnown {
		t.Errorf("msg_1 wrong: %+v", first)
	}
	if s.Requests[1].MessageID != "msg_2" || s.Requests[1].Tokens.TTLKnown || s.Requests[1].Model != "claude-fable-5-1" {
		t.Errorf("msg_2 should have no TTL breakdown: %+v", s.Requests[1])
	}
	sub := s.Requests[2]
	if sub.MessageID != "msg_9" || !sub.IsSubagent || sub.AgentID != "abc" {
		t.Errorf("subagent request wrong: %+v", sub)
	}
	refused, answered := s.Requests[3], s.Requests[4]
	if refused.MessageID != "msg_4" || refused.Iteration != 0 || refused.Model != "claude-fable-5" || refused.Tokens.Output != 10 || refused.Tokens.CacheWrite1h != 5 || refused.ToolUses != 0 {
		t.Errorf("refused attempt of the fallback message wrong: %+v", refused)
	}
	if answered.MessageID != "msg_4" || answered.Iteration != 1 || answered.Model != "claude-opus-4-8" || answered.Tokens.Output != 20 || answered.Tokens.CacheWrite1h != 6 {
		t.Errorf("answering attempt of the fallback message wrong: %+v", answered)
	}
	if !s.StartedAt.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)) || !s.EndedAt.Equal(time.Date(2026, 9, 1, 10, 0, 8, 700_000_000, time.UTC)) {
		t.Errorf("time span wrong: %v .. %v", s.StartedAt, s.EndedAt)
	}
}

func TestReadEnvironmentAndCostReferences(t *testing.T) {
	cfg := t.TempDir()
	claude := `{"mcpServers":{"agentsmemory":{"type":"http"}},"projects":{"/w/proj":{"lastCost":3.5,"lastSessionId":"s1","lastTotalInputTokens":10,"lastTotalOutputTokens":20,"lastTotalCacheCreationInputTokens":30,"lastTotalCacheReadInputTokens":40,"lastDuration":5000},"/w/nocost":{"lastSessionId":"s2"}}}`
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugins := `{"version":2,"plugins":{"quality-harness@quality-harness":[{"scope":"user"}],"eidos@eidos":[]}}`
	if err := os.WriteFile(filepath.Join(cfg, "plugins", "installed_plugins.json"), []byte(plugins), 0o644); err != nil {
		t.Fatal(err)
	}
	env := ReadEnvironment(cfg, time.Now())
	if !env.HasAgentsmemory || !env.HasQualityHarness || env.Label() != "qam-installed" {
		t.Errorf("environment = %+v", env)
	}
	refs, err := ReadCostReferences(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].SessionID != "s1" || refs[0].LastCost != 3.5 || refs[0].CacheRead != 40 || refs[0].CWD != "/w/proj" {
		t.Errorf("cost references = %+v", refs)
	}
}

func TestEnvironmentAndReferencesTolerateAMissingClaudeJSON(t *testing.T) {
	cfg := t.TempDir()
	env := ReadEnvironment(cfg, time.Now())
	if env.Label() != "plain" {
		t.Errorf("label = %q, want plain", env.Label())
	}
	refs, err := ReadCostReferences(cfg)
	if err != nil || refs != nil {
		t.Errorf("refs=%v err=%v, want none and no error", refs, err)
	}
}

func TestDiscoverConfigDirsHonoursTheEnvironmentVariable(t *testing.T) {
	cfg := writeFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dirs := DiscoverConfigDirs()
	if len(dirs) == 0 || dirs[0] != filepath.Clean(cfg) {
		t.Errorf("DiscoverConfigDirs() = %v, want %s first", dirs, cfg)
	}
	for i := 1; i < len(dirs); i++ {
		if strings.HasPrefix(dirs[i], cfg) {
			t.Errorf("config dir listed twice: %v", dirs)
		}
	}
}
