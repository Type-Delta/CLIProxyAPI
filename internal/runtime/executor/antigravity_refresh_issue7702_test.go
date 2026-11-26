package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestIssue7702AntigravityPostRefreshCreditsFollowsLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := issue6199AntigravityAuth(t.Name(), t.Name()+"-refresh")
		issue6199CreditsLifecycleFixture(t, auth.ID)

		var creditsRequest context.Context
		creditsCalls := 0
		var stalled atomic.Bool
		stalled.Store(true)
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://oauth2.googleapis.com/token":
				return issue6199AntigravityJSONResponse(req, `{"access_token":"new-access","expires_in":3600}`), nil
			case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
				creditsCalls++
				if !stalled.Load() {
					return issue6199CreditsBalanceResponse(req, 900), nil
				}
				creditsRequest = req.Context()
				// Complete the HTTP handshake and stall the response body.
				body, writer := io.Pipe()
				context.AfterFunc(req.Context(), func() { _ = writer.CloseWithError(req.Context().Err()) })
				return &http.Response{StatusCode: http.StatusOK, Body: body, Request: req}, nil
			default:
				return nil, fmt.Errorf("unexpected intercepted request: %s", req.URL)
			}
		})
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
		lifecycle, cancelLifecycle := context.WithCancel(ctx)
		defer cancelLifecycle()
		executor := newIssue6199AntigravityCreditsExecutor()

		updated := issue6199RefreshForCreditsLifecycle(t, executor, lifecycle, auth)
		if updated == nil {
			t.Fatal("token refresh did not complete")
		}
		if creditsCalls != 1 {
			t.Fatalf("post-refresh credits calls = %d, want 1", creditsCalls)
		}
		if creditsRequest == nil {
			t.Fatal("post-refresh credits request did not start")
		}
		if _, hasDeadline := creditsRequest.Deadline(); hasDeadline {
			t.Error("post-refresh credits request has a response deadline")
		}

		// Advancing past the former timeout must leave the read alone.
		time.Sleep(antigravityCreditsHintRefreshTimeout + time.Nanosecond)
		synctest.Wait()
		if creditsRequest.Err() != nil {
			t.Errorf("credits request stopped before lifecycle cancellation: %v", creditsRequest.Err())
		}
		cancelLifecycle()
		synctest.Wait()

		stateVal, ok := antigravityCreditsHintRefreshByID.Load(auth.ID)
		if !ok {
			t.Fatal("expected credits refresh state to be registered for auth")
		}
		state, ok := stateVal.(*antigravityCreditsHintRefreshState)
		if !ok || state == nil {
			t.Fatalf("unexpected credits refresh state type %T", stateVal)
		}
		state.mu.Lock()
		remaining := state.task
		state.mu.Unlock()
		if remaining != nil {
			t.Fatal("stalled post-refresh credits task was not released after lifecycle cancellation; subsequent refreshes are deadlocked")
		}

		// A new lifecycle can reserve the same slot immediately.
		stalled.Store(false)
		recovered := issue6199AntigravityAuth(t.Name(), t.Name()+"-refresh")
		issue6199RefreshForCreditsLifecycle(t, executor, ctx, recovered)
		synctest.Wait()
		issue6199AssertCreditsBalance(t, updated, 900)
	})
}
