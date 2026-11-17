package auth

import (
	"context"
	"testing"
	"time"
)

func TestQuotaReserveProbeStartedBeforeResetCannotRestoreHold(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "probe-reset", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		manager.runQuotaReserveCheck(context.Background(), auth, func(context.Context, *Auth) ([]QuotaReserveBar, error) {
			close(started)
			<-release
			return []QuotaReserveBar{{ID: "weekly", RemainingPercent: 0, ResetAt: time.Now().Add(time.Hour)}}, nil
		})
	}()
	<-started
	if _, err := manager.ApplyProviderQuotaReset(context.Background(), auth.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if manager.quotaReserveHolds(auth, "") {
		t.Fatal("stale probe restored reserve hold")
	}
	manager.ApplyQuotaReserveBarsWithContext(manager.CaptureQuotaObservation(context.Background(), auth.ID), auth, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 0}})
	if !manager.quotaReserveHolds(auth, "") {
		t.Fatal("fresh observation was rejected")
	}
}
