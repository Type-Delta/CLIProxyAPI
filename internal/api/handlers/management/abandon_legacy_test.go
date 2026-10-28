package management

import "testing"

func TestR2RepairCanAbandonLegacyImport(t *testing.T) {
	maintenance := &analyticsV2MaintenanceFake{}
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	h := v2HandlerWithReader(reader, maintenance)
	c, w := v2Request("POST", "/v8/management/analytics/repair", `{"kind":"abandon_legacy_import","reason":"corrupt source"}`)
	h.RepairAnalytics(c)
	if w.Code != 202 || maintenance.request.Kind != "abandon_legacy_import" || maintenance.request.Options["reason"] != "corrupt source" {
		t.Fatalf("response=%d %s request=%+v", w.Code, w.Body, maintenance.request)
	}
	c, w = v2Request("POST", "/v8/management/analytics/repair", `{"kind":"abandon_legacy_import","reason":"  "}`)
	h.RepairAnalytics(c)
	if w.Code != 400 {
		t.Fatalf("missing reason: %d %s", w.Code, w.Body)
	}
}
