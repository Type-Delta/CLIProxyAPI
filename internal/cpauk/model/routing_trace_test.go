package model

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestSanitizeRoutingTraceKeepsEmptyCandidatesAsArray(t *testing.T) {
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, DecidedBy: routing.DecidedByNone, Candidates: []routing.Candidate{}, Pick: -1}
	encoded, truncated := SanitizeRoutingTrace(trace, [32]byte{}, true, "codex")
	if truncated {
		t.Fatal("small trace reported as truncated")
	}
	if !strings.Contains(string(encoded), `"candidates":[]`) {
		t.Fatalf("persisted trace = %s, want candidates as an empty array", encoded)
	}
}
