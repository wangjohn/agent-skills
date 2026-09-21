package archive

import (
	"testing"
	"time"
)

func TestSessionClosurePreservesLastObservedTurnOutcome(t *testing.T) {
	at := time.Now().UTC()
	for _, tc := range []struct {
		event, provenance, status string
		want                      TurnOutcome
	}{
		{"StopFailure", "hook:claude:stopfailure", "", TurnOutcomeError},
		{"Interrupt", "hook:codex:interrupt", "", TurnOutcomeInterrupted},
		{"stop", "hook:cursor:stop", "completed", TurnOutcomeCompleted},
	} {
		events := []SupplementalEvidence{
			{Kind: EvidenceKindLifecycleHook, ObservedAt: at, Provenance: tc.provenance, Payload: map[string]any{"event_name": tc.event, "status": tc.status}},
			{Kind: EvidenceKindLifecycleHook, ObservedAt: at.Add(time.Second), Provenance: "hook:claude:sessionend", Payload: map[string]any{"event_name": "SessionEnd"}},
		}
		state, outcome := deriveLifecycle(events)
		if state != MetadataStateClosed || outcome != tc.want {
			t.Fatalf("%s then close = %s/%s", tc.event, state, outcome)
		}
	}
}
