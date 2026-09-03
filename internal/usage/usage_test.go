package usage

import "testing"

func TestClassifyRecognisesEachQAMComponent(t *testing.T) {
	cases := []struct {
		name, command, skill, subagent string
		want                           Signals
	}{
		{name: "mcp__agentsmemory__am_search", want: Signals{AM: 1}},
		{name: "mcp__agentsmemory__am_kg_add", want: Signals{AM: 1}},
		{name: "mcp__plugin_mempalace_mempalace__mempalace_search", want: Signals{}},
		{name: "Bash", command: "mrw read a.go:1-8 b.go", want: Signals{MRW: 1}},
		{name: "Bash", command: "cd /repo && mrw -C /repo write plan.mrw", want: Signals{MRW: 1}},
		{name: "Bash", command: "/usr/local/bin/mrw check ./internal", want: Signals{MRW: 1}},
		{name: "Bash", command: "go build -o bin/mrw ./cmd/mrw", want: Signals{}},
		{name: "Bash", command: "echo mrw-skill loaded", want: Signals{}},
		{name: "Bash", command: "adr-lint docs/adr/ADR-001.md", want: Signals{QH: 1}},
		{name: "Bash", command: "cd x && qh-root && adr-verify --task T1", want: Signals{QH: 1}},
		{name: "Bash", command: "mrw write --check plan.mrw && adr-lint docs/adr", want: Signals{MRW: 1, QH: 1}},
		{name: "Bash", command: "git status", want: Signals{}},
		{name: "Skill", skill: "quality-harness:adr-write", want: Signals{QH: 1}},
		{name: "Skill", skill: "effective-go", want: Signals{}},
		{name: "Agent", subagent: "quality-harness:review", want: Signals{QH: 1}},
		{name: "Agent", subagent: "general-purpose", want: Signals{}},
		{name: "Read", want: Signals{}},
	}
	for _, c := range cases {
		got := Classify(c.name, c.command, c.skill, c.subagent)
		if got != c.want {
			t.Errorf("Classify(%q, %q, %q, %q) = %+v, want %+v", c.name, c.command, c.skill, c.subagent, got, c.want)
		}
	}
}

func TestCohortNamesComponentsInQAMOrder(t *testing.T) {
	cases := []struct {
		s    Signals
		want string
	}{
		{Signals{}, "none"},
		{Signals{AM: 3}, "partial:A"},
		{Signals{MRW: 1}, "partial:M"},
		{Signals{QH: 1}, "partial:Q"},
		{Signals{AM: 1, MRW: 1}, "partial:AM"},
		{Signals{QH: 1, MRW: 1}, "partial:QM"},
		{Signals{QH: 2, AM: 5, MRW: 1}, "QAM"},
	}
	for _, c := range cases {
		if got := c.s.Cohort(); got != c.want {
			t.Errorf("%+v.Cohort() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestTokensMaxKeepsTheLargerCountAndAnyKnownTTL(t *testing.T) {
	a := Tokens{Input: 2, Output: 1, CacheRead: 10, CacheWrite: 5}
	b := Tokens{Input: 2, Output: 90, CacheRead: 10, CacheWrite: 5, CacheWrite1h: 5, TTLKnown: true, Thinking: 30, ThinkingKnown: true}
	a.Max(b)
	if a.Output != 90 || a.CacheWrite1h != 5 || !a.TTLKnown || a.Thinking != 30 || !a.ThinkingKnown {
		t.Fatalf("Max merged wrongly: %+v", a)
	}
}

func TestEnvironmentLabel(t *testing.T) {
	cases := []struct {
		e    Environment
		want string
	}{
		{Environment{}, "plain"},
		{Environment{HasAgentsmemory: true}, "agentsmemory-only"},
		{Environment{HasQualityHarness: true}, "quality-harness-only"},
		{Environment{HasAgentsmemory: true, HasQualityHarness: true}, "qam-installed"},
	}
	for _, c := range cases {
		if got := c.e.Label(); got != c.want {
			t.Errorf("%+v.Label() = %q, want %q", c.e, got, c.want)
		}
	}
}
