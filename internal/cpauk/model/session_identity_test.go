package model

import "testing"

func TestSplitSessionIdentity(t *testing.T) {
	for _, test := range []struct{ value, id, source string }{
		{"header:raw-session", "raw-session", "header"},
		{"codex:uuid:agent:child", "uuid:agent:child", "codex"},
		{"claude:uuid", "uuid", "claude"},
		{"custom:raw", "custom:raw", ""},
	} {
		id, source := SplitSessionIdentity(test.value)
		if id != test.id || source != test.source {
			t.Errorf("split %q = %q,%q", test.value, id, source)
		}
	}
}
