package model

import "strings"

// SessionSources are the namespaces assigned by CPA's session identity pipeline.
// Storage retains them to distinguish the same client ID used by different sources.
var SessionSources = [...]string{"claude", "codex", "agy", "header", "affinity", "slot", "task", "conv", "thread", "clientreq", "geminicache", "session", "pck", "user", "execution"}

// SplitSessionIdentity removes only a recognized CPA namespace, preserving colons
// inside the client's own identifier and unknown identity formats.
func SplitSessionIdentity(value string) (id, source string) {
	for _, candidate := range SessionSources {
		if raw, ok := strings.CutPrefix(value, candidate+":"); ok {
			return raw, candidate
		}
	}
	return value, ""
}

// IsDerivedSessionIdentity recognizes internal routing/correlation identities,
// not identifiers explicitly supplied in a client's session header or body.
func IsDerivedSessionIdentity(value string) bool {
	for _, prefix := range []string{"derived:", "ctx:", "lcp:"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
