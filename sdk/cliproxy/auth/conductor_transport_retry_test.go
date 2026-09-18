package auth

import (
	"context"
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func registerTransportRetryAuth(t *testing.T, m *Manager, id, model string) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
}

func TestManager_ShouldRetryAfterError_RetriesStatusLessFailures(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(1, 30*time.Second, 0)
	model := "transport-retry-" + uuid.NewString()
	registerTransportRetryAuth(t, m, "transport-retry-auth", model)

	reset := &url.Error{Op: "Post", URL: "https://example.com", Err: errors.New("connection reset by peer")}
	cases := []struct {
		name string
		err  error
	}{
		{name: "url error", err: reset},
		{name: "http2 body closed", err: errors.New("http2: response body closed")},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, maxWait := m.retrySettings()
			wait, shouldRetry := m.shouldRetryAfterError(tc.err, 0, []string{"codex"}, model, maxWait)
			if !shouldRetry || wait != 0 {
				t.Fatalf("status-less failure retry = (%v, %t), want (0, true)", wait, shouldRetry)
			}
			if _, shouldRetry = m.shouldRetryAfterError(tc.err, 1, []string{"codex"}, model, maxWait); shouldRetry {
				t.Fatalf("status-less failure retried after the configured additional round")
			}
		})
	}
}

func TestManager_ShouldRetryAfterError_DoesNotRetryCanceledOrDeadline(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second, 0)
	model := "transport-cancel-" + uuid.NewString()
	registerTransportRetryAuth(t, m, "transport-cancel-auth", model)

	cases := []error{
		context.Canceled,
		context.DeadlineExceeded,
		&url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
	}
	for _, err := range cases {
		_, _, maxWait := m.retrySettings()
		if _, shouldRetry := m.shouldRetryAfterError(err, 0, []string{"codex"}, model, maxWait); shouldRetry {
			t.Fatalf("expected shouldRetry=false for %v", err)
		}
	}
}

func TestManager_ShouldRetryAfterError_WaitsForStatusLessCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	model := "transport-cooldown-" + uuid.NewString()
	next := time.Now().Add(2 * time.Second)
	auth := &Auth{
		ID:       "transport-cooldown-auth",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
				LastError:      &Error{Message: "http2: response body closed"},
			},
		},
	}
	registerTransportRetryAuth(t, m, auth.ID, model)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	m.SetRetryConfig(1, 30*time.Second, 0)
	_, _, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(errors.New("http2: response body closed"), 0, []string{"codex"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true while a status-less cooldown is pending")
	}
	if wait <= 0 || wait > 2*time.Second {
		t.Fatalf("expected wait within (0, 2s], got %v", wait)
	}

	m.SetRetryConfig(1, time.Second, 0)
	_, _, maxWait = m.retrySettings()
	if _, shouldRetry = m.shouldRetryAfterError(errors.New("http2: response body closed"), 0, []string{"codex"}, model, maxWait); shouldRetry {
		t.Fatalf("expected shouldRetry=false when the status-less cooldown exceeds max-retry-interval")
	}
}

func TestManager_MarkResult_TransportFailureDoesNotCooldown(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	prevTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(prevTransient) })

	m := NewManager(nil, nil, nil)
	model := "transport-cooldown-skip-" + uuid.NewString()
	auth := &Auth{ID: "transport-cooldown-skip-auth", Provider: "codex"}
	registerTransportRetryAuth(t, m, auth.ID, model)

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error:    resultErrorFromError(&url.Error{Op: "Post", URL: "https://example.com", Err: errors.New("connection refused")}),
	})

	assertNoCooldown(t, m, auth.ID, model)
}
