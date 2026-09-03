package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/statsv1/internal/usage"
)

func sample() usage.Session {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	return usage.Session{
		ID: "s1", ConfigDir: "/cfg", ProjectSlug: "-w-proj", CWD: "/w/proj", TranscriptPath: "/cfg/projects/-w-proj/s1.jsonl", Version: "2.1.236",
		StartedAt: at, EndedAt: at.Add(time.Minute), UserTurns: 2, ToolCalls: 3, Subagents: 1,
		Signals: usage.Signals{AM: 1, MRW: 1, QH: 1},
		Requests: []usage.Request{
			{SessionID: "s1", MessageID: "msg_1", RequestID: "req_1", At: at, Model: "claude-opus-5", Tokens: usage.Tokens{Input: 2, Output: 90, CacheRead: 1000, CacheWrite: 100, CacheWrite1h: 100, TTLKnown: true, Thinking: 30, ThinkingKnown: true}, ToolUses: 1},
			{SessionID: "s1", MessageID: "msg_9", RequestID: "req_3", At: at.Add(time.Second), Model: "claude-opus-5", Tokens: usage.Tokens{Input: 1, Output: 2, CacheRead: 4, CacheWrite: 3}, IsSubagent: true, AgentID: "abc"},
		},
	}
}

func TestRoundTripIsIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	env := usage.Environment{Dir: "/cfg", HasAgentsmemory: true, ScannedAt: time.Now()}
	refs := []usage.CostReference{{ConfigDir: "/cfg", CWD: "/w/proj", SessionID: "s1", LastCost: 1.5, Input: 3, Output: 92}}
	for i := 0; i < 2; i++ {
		if err := st.PutEnvironment(env); err != nil {
			t.Fatal(err)
		}
		if err := st.PutSession(sample()); err != nil {
			t.Fatal(err)
		}
		if err := st.PutCostReferences(refs); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 after collecting twice", len(sessions))
	}
	got := sessions[0]
	want := sample()
	if got.ID != want.ID || got.CWD != want.CWD || got.UserTurns != want.UserTurns || got.Signals != want.Signals || !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("session round trip changed it: %+v", got)
	}
	if len(got.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(got.Requests))
	}
	if got.Requests[0] != want.Requests[0] || got.Requests[1] != want.Requests[1] {
		t.Errorf("requests round trip changed them:\n got %+v\nwant %+v", got.Requests, want.Requests)
	}
	envs, err := st.Environments()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := envs["/cfg"]; !ok || !e.HasAgentsmemory || e.Label() != "agentsmemory-only" {
		t.Errorf("environments = %+v", envs)
	}
	gotRefs, err := st.CostReferences()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRefs) != 1 || gotRefs[0].LastCost != 1.5 || gotRefs[0].Output != 92 {
		t.Errorf("cost references = %+v", gotRefs)
	}
}

func TestReopeningAnExistingDatabaseKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(sample()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sessions, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || len(sessions[0].Requests) != 2 {
		t.Errorf("rows lost across reopen: %+v", sessions)
	}
}
