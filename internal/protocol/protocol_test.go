package protocol

import "testing"

func TestTurnsFor(t *testing.T) {
	limits := AgentLimits{MaxTurns: 10, MaxTaskTurns: 40}
	cases := []struct {
		mode string
		want int
	}{
		{"task", 0},
		{"direct", 40},
		{"", 40},
		{"scheduled", 40},
		{"watch", 10},
	}
	for _, c := range cases {
		if got := TurnsFor(limits, c.mode); got != c.want {
			t.Errorf("TurnsFor(%q) = %d, want %d", c.mode, got, c.want)
		}
	}
	// Zeros fall back to platform defaults.
	if got := TurnsFor(AgentLimits{}, "direct"); got != 128 {
		t.Errorf("default direct = %d", got)
	}
	if got := TurnsFor(AgentLimits{}, "heartbeat"); got != 16 {
		t.Errorf("default ambient = %d", got)
	}
	NopLogger("x", nil) // never panics
}
