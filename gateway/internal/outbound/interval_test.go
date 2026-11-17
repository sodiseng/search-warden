package outbound

import (
	"context"
	"testing"
	"time"
)

// Found in acceptance (2026-11-10): after lowering egress.health_interval_seconds,
// checks kept their old schedule until it ran out (up to an hour).
func TestShorterHealthIntervalAppliesAtOnce(t *testing.T) {
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	up := upstream(t)
	reg := registry(t)
	r := runtimeFor(t, []string{up.URL}, []string{up.URL}, func() time.Time { return clock })
	s := snapshot(t, reg, testKey(t, reg, "tvly-a", "direct"))
	s.Config.Egress.HealthIntervalSeconds = 3600
	r.Apply(s)
	for _, j := range r.due() {
		r.probeEgress(context.Background(), j)
	}
	clock = clock.Add(5 * time.Minute)
	if n := len(r.due()); n != 0 {
		t.Fatalf("%d probes due 5 minutes into an hourly schedule", n)
	}
	s2 := snapshot(t, reg, testKey(t, reg, "tvly-a", "direct"))
	s2.Config.Egress.HealthIntervalSeconds = 10
	r.Apply(s2)
	clock = clock.Add(11 * time.Second)
	if n := len(r.due()); n != 1 {
		t.Fatalf("%d probes due 11 s after the interval became 10 s, want 1", n)
	}
}
