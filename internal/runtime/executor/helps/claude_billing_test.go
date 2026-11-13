package helps

import "testing"

func TestClaudeBillingFingerprintNativeCaptures(t *testing.T) {
	for _, sample := range []struct{ prompt, version, want string }{
		{"CPA_OFFICIAL_BASEURL_CLI_SYSTEM_EMPTY_b82d4e", "2.1.258", "1f4"},
		{"Reply only OK.", "2.1.322", "b48"},
	} {
		if got := ClaudeBillingFingerprint(sample.prompt, sample.version); got != sample.want {
			t.Fatalf("native fingerprint for%s = %s, want%s", sample.version, got, sample.want)
		}
	}
}
