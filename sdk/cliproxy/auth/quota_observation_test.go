package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
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

func TestRoutingQuotaObservationRejectsOlderProbe(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "old-routing-probe", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	now := time.Now()
	newQuota := QuotaState{ObservedAt: now, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "20"}}
	if _, errUpdate := manager.UpdateQuotaObservation(ctx, auth.ID, newQuota); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	probeCtx := manager.CaptureQuotaObservation(ctx, auth.ID)
	seq := manager.nextExecutionSeq()
	oldQuota := QuotaState{ObservedAt: now.Add(-time.Minute), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "90"}}
	updated, errUpdate := manager.UpdateQuotaObservation(probeCtx, auth.ID, oldQuota)
	if errUpdate != nil || !updated.Quota.ObservedAt.Equal(now) {
		t.Fatalf("older probe replaced a newer observation: %v, %v", updated, errUpdate)
	}

	if _, errReset := manager.ApplyProviderQuotaReset(ctx, auth.ID, nil, nil); errReset != nil {
		t.Fatal(errReset)
	}
	oldQuota.ObservedAt = now.Add(time.Minute)
	updated, errUpdate = manager.UpdateQuotaObservation(probeCtx, auth.ID, oldQuota)
	if errUpdate != nil || !updated.Quota.ObservedAt.IsZero() || len(updated.Quota.Signals) != 0 {
		t.Fatalf("probe from before the reset updated the quota: %v, %v", updated, errUpdate)
	}
	responseCtx := internallogging.WithResponseHeadersHolder(ctx)
	internallogging.SetResponseHeaders(responseCtx, http.Header{"X-Codex-Primary-Used-Percent": []string{"90"}})
	manager.MarkResult(responseCtx, Result{AuthID: auth.ID, Provider: "codex", Success: true, ExecutionSeq: seq})
	updated, _ = manager.GetByID(auth.ID)
	if len(updated.Quota.Signals) != 0 {
		t.Fatalf("generation from before the reset updated the quota: %v", updated.Quota.Signals)
	}
}

func TestRoutingQuotaObservationRejectsCredentialReplacement(t *testing.T) {
	for _, replace := range []string{"update", "remove-register"} {
		t.Run(replace, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, nil, nil)
			first, errRegister := manager.Register(ctx, &Auth{ID: "replaced-probe", Provider: "codex", Attributes: map[string]string{"api_key": "original"}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			probeCtx := manager.CaptureQuotaObservationForAuth(ctx, first)
			second := first.Clone()
			second.Attributes["api_key"] = "replacement"
			if replace == "update" {
				_, errRegister = manager.Update(ctx, second)
			} else {
				manager.Remove(ctx, first.ID)
				_, errRegister = manager.Register(ctx, second)
			}
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			quota := QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "90"}}
			for _, stale := range []context.Context{probeCtx, manager.CaptureQuotaObservationForAuth(ctx, first)} {
				updated, errUpdate := manager.UpdateQuotaObservation(stale, first.ID, quota)
				if errUpdate != nil || len(updated.Quota.Signals) != 0 {
					t.Fatalf("probe for replaced credential updated the quota: %v, %v", updated, errUpdate)
				}
			}
		})
	}
}
