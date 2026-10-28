package model

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

// SanitizeRoutingTrace builds the bounded persisted form of a routing decision.
func SanitizeRoutingTrace(trace *routing.Trace, identityKey [32]byte, storeCredential bool, eventProvider string) (json.RawMessage, bool) {
	if trace == nil {
		return nil, false
	}
	clone := *trace
	clone.Candidates = append(make([]routing.Candidate, 0, len(trace.Candidates)), trace.Candidates...)
	for index := range clone.Candidates {
		candidate := &clone.Candidates[index]
		candidate.Label = ""
		if storeCredential {
			provider := candidate.Provider
			if strings.TrimSpace(provider) == "" {
				provider = eventProvider
			}
			identity, err := CredentialID(identityKey[:], provider, candidate.AuthIndex, candidate.ID)
			if err != nil || identity == nil {
				candidate.ID = ""
			} else {
				candidate.ID = *identity
			}
		} else {
			candidate.ID = ""
		}
	}
	encoded, err := json.Marshal(clone)
	if err == nil && len(encoded) > MaxRoutingTraceBytes {
		for index := range clone.Candidates {
			clone.Candidates[index].Provider = ""
			clone.Candidates[index].Gate = ""
			clone.Candidates[index].UntilMs = 0
			clone.Candidates[index].Priority = 0
		}
		encoded, err = json.Marshal(clone)
	}
	if err != nil || len(encoded) > MaxRoutingTraceBytes {
		return nil, true
	}
	return json.RawMessage(encoded), false
}
