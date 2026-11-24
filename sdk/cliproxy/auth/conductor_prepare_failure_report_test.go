package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type prepareFailureReport struct {
	authID string
	model  string
	err    error
}

type prepareFailureReportingExecutor struct {
	claudeCancellationTestExecutor
	mu      sync.Mutex
	reports []prepareFailureReport
}

func (e *prepareFailureReportingExecutor) ReportPrepareFailure(_ context.Context, auth *Auth, model string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reports = append(e.reports, prepareFailureReport{authID: auth.ID, model: model, err: err})
}

// A credential that fails request auth preparation never reaches the executor, so the
// conductor must hand the failure to the executor's reporter before trying the next one.
func TestManagerReportsPrepareFailureToExecutor(t *testing.T) {
	errPrepare := errors.New("token refresh failed")
	tests := []struct {
		name        string
		run         func(context.Context, *Manager, string) error
		wantReports int
	}{
		{name: "execute", wantReports: 1, run: func(ctx context.Context, manager *Manager, model string) error {
			_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
		{name: "stream", wantReports: 1, run: func(ctx context.Context, manager *Manager, model string) error {
			_, err := manager.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
			return err
		}},
		// Token counts publish no analytics attempts.
		{name: "count tokens", wantReports: 0, run: func(ctx context.Context, manager *Manager, model string) error {
			_, err := manager.ExecuteCount(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &prepareFailureReportingExecutor{}
			executor.prepareFn = func(context.Context, *Auth) (*Auth, error) { return nil, errPrepare }
			manager, auth, model := newClaudeCancellationTestManager(t, &executor.claudeCancellationTestExecutor, nil)
			manager.RegisterExecutor(executor)

			if err := tt.run(context.Background(), manager, model); !errors.Is(err, errPrepare) {
				t.Fatalf("error = %v, want prepare failure", err)
			}
			executor.mu.Lock()
			defer executor.mu.Unlock()
			if len(executor.reports) != tt.wantReports {
				t.Fatalf("reports = %d, want %d", len(executor.reports), tt.wantReports)
			}
			for _, report := range executor.reports {
				if report.authID != auth.ID || report.model != model || !errors.Is(report.err, errPrepare) {
					t.Fatalf("report = %+v, want auth %s model %s", report, auth.ID, model)
				}
			}
		})
	}
}
