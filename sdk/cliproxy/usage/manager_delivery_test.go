package usage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type pluginFunc func(context.Context, Record)

func (fn pluginFunc) HandleUsage(ctx context.Context, record Record) {
	fn(ctx, record)
}

func TestManagerDropsNewestAtTotalQueueCapacity(t *testing.T) {
	manager := NewManager(2)
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var delivered []string
	if _, errRegister := manager.RegisterNamed("blocked", pluginFunc(func(_ context.Context, record Record) {
		if record.Alias == "first" {
			close(started)
			<-release
		}
		mu.Lock()
		delivered = append(delivered, record.Alias)
		mu.Unlock()
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}

	manager.Publish(context.Background(), Record{Alias: "first"})
	waitChannel(t, started, "first observer call")
	manager.Publish(context.Background(), Record{Alias: "second"})
	manager.Publish(context.Background(), Record{Alias: "third"})
	manager.Publish(context.Background(), Record{Alias: "newest"})
	if stats := manager.Stats(); stats.QueueDepth != 2 || stats.Dropped != 1 {
		t.Fatalf("saturated stats = %+v, want depth 2 and one drop", stats)
	}
	close(release)
	closeManager(t, manager)

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("delivered = %v, want oldest three records", got)
	}
}

func TestManagerObserverLanesAreIndependent(t *testing.T) {
	manager := NewManager(8)
	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	fastCalled := make(chan struct{})
	if _, errRegister := manager.RegisterNamed("slow", pluginFunc(func(context.Context, Record) {
		close(slowStarted)
		<-slowRelease
	})); errRegister != nil {
		t.Fatalf("register slow observer: %v", errRegister)
	}
	if _, errRegister := manager.RegisterNamed("fast", pluginFunc(func(context.Context, Record) {
		close(fastCalled)
	})); errRegister != nil {
		t.Fatalf("register fast observer: %v", errRegister)
	}

	manager.Publish(context.Background(), Record{})
	waitChannel(t, slowStarted, "slow observer start")
	waitChannel(t, fastCalled, "fast observer while slow lane is blocked")
	close(slowRelease)
	closeManager(t, manager)
}

func TestManagerRecoversObserverAndSanitizerPanics(t *testing.T) {
	manager := NewManager(8)
	if _, errRegister := manager.RegisterSanitizerTapNamed("panic-tap", pluginFunc(func(context.Context, Record) {
		panic("tap secret must not appear in manager state")
	})); errRegister != nil {
		t.Fatalf("register sanitizer tap: %v", errRegister)
	}
	var observerCalls atomic.Int64
	if _, errRegister := manager.RegisterNamed("panic-observer", pluginFunc(func(context.Context, Record) {
		if observerCalls.Add(1) == 1 {
			panic("observer secret must not appear in manager state")
		}
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}

	manager.Publish(context.Background(), Record{})
	manager.Publish(context.Background(), Record{})
	closeManager(t, manager)
	stats := manager.Stats()
	if stats.TapPanics != 2 || stats.TapDropped != 2 || observerCalls.Load() != 2 {
		t.Fatalf("panic stats = %+v calls=%d", stats, observerCalls.Load())
	}
}

func TestManagerAccountingTapObserverOrder(t *testing.T) {
	manager := NewManager(8)
	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}
	if _, errRegister := manager.RegisterAccountingNamed("limits", pluginFunc(func(context.Context, Record) {
		appendOrder("accounting")
	})); errRegister != nil {
		t.Fatalf("register accounting: %v", errRegister)
	}
	if _, errRegister := manager.RegisterSanitizerTapNamed("analytics", pluginFunc(func(context.Context, Record) {
		appendOrder("tap")
	})); errRegister != nil {
		t.Fatalf("register tap: %v", errRegister)
	}
	if _, errRegister := manager.RegisterNamed("observer", pluginFunc(func(context.Context, Record) {
		appendOrder("observer")
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}

	manager.Publish(context.Background(), Record{})
	closeManager(t, manager)
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "accounting,tap,observer" {
		t.Fatalf("delivery order = %q", got)
	}
}

func TestManagerUnregisterWaitsForActiveCallback(t *testing.T) {
	manager := NewManager(8)
	started := make(chan struct{})
	release := make(chan struct{})
	unregister, errRegister := manager.RegisterNamed("slow", pluginFunc(func(context.Context, Record) {
		close(started)
		<-release
	}))
	if errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	manager.Publish(context.Background(), Record{})
	waitChannel(t, started, "observer callback")

	unregistered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		unregistered <- unregister(ctx)
	}()
	select {
	case errUnregister := <-unregistered:
		t.Fatalf("unregister returned during active callback: %v", errUnregister)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if errUnregister := <-unregistered; errUnregister != nil {
		t.Fatalf("unregister: %v", errUnregister)
	}
	manager.Publish(context.Background(), Record{})
	closeManager(t, manager)
}

func TestManagerCloseHonorsDeadlineAndAbandonsQueuedRecords(t *testing.T) {
	manager := NewManager(4)
	started := make(chan struct{})
	release := make(chan struct{})
	if _, errRegister := manager.RegisterNamed("blocked", pluginFunc(func(context.Context, Record) {
		close(started)
		<-release
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	manager.Publish(context.Background(), Record{})
	waitChannel(t, started, "blocked observer")
	manager.Publish(context.Background(), Record{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if errClose := manager.Close(ctx); !errorsIsDeadline(errClose) {
		t.Fatalf("close error = %v, want deadline", errClose)
	}
	if stats := manager.Stats(); stats.QueueDepth != 0 || stats.Dropped == 0 {
		t.Fatalf("post-deadline stats = %+v, want abandoned queue", stats)
	}
	close(release)
}

func TestManagerCanRestartAfterStop(t *testing.T) {
	manager := NewManager(8)
	var calls atomic.Int64
	if _, errRegister := manager.RegisterNamed("counter", pluginFunc(func(context.Context, Record) {
		calls.Add(1)
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	for cycle := 0; cycle < 3; cycle++ {
		manager.Start(context.Background())
		manager.Publish(context.Background(), Record{})
		closeManager(t, manager)
	}
	if calls.Load() != 3 {
		t.Fatalf("observer calls = %d, want 3", calls.Load())
	}
}

func TestManagerFreezesAndTruncatesGenericSnapshot(t *testing.T) {
	manager := NewManager(8)
	release := make(chan struct{})
	result := make(chan Record, 1)
	contextResult := make(chan string, 1)
	if _, errRegister := manager.RegisterNamed("snapshot", pluginFunc(func(ctx context.Context, record Record) {
		<-release
		result <- record
		contextResult <- EndpointClassFromContext(ctx)
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	headers := http.Header{"X-Test": {"before"}}
	ctx := WithEndpointClass(context.Background(), "responses")
	manager.Publish(ctx, Record{Provider: strings.Repeat("p", 6000), Fail: Failure{Body: strings.Repeat("b", 6000)}, ResponseHeaders: headers})
	headers.Set("X-Test", "after")
	ctx = WithEndpointClass(ctx, "changed")
	close(release)
	closeManager(t, manager)

	got := <-result
	if got.ResponseHeaders.Get("X-Test") != "before" {
		t.Fatalf("snapshot header = %q, want before", got.ResponseHeaders.Get("X-Test"))
	}
	if gotBytes := len(got.Provider) + len(got.Fail.Body); gotBytes > MaxObserverSnapshotBytes {
		t.Fatalf("snapshot strings use %d bytes, maximum %d", gotBytes, MaxObserverSnapshotBytes)
	}
	if endpoint := <-contextResult; endpoint != "responses" {
		t.Fatalf("snapshot endpoint = %q", endpoint)
	}
}

func TestManagerAssignsObservedAndUniqueSyntheticRequestIDs(t *testing.T) {
	manager := NewManager(8)
	records := make(chan Record, 3)
	if _, errRegister := manager.RegisterNamed("capture", pluginFunc(func(_ context.Context, record Record) {
		records <- record
	})); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	observed := "d1371f43e6b8362d05d7567ed5fcc2ad"
	manager.Publish(WithProxyRequestID(context.Background(), observed), Record{})
	manager.Publish(context.Background(), Record{})
	manager.Publish(context.Background(), Record{})
	closeManager(t, manager)

	first, second, third := <-records, <-records, <-records
	if first.ProxyRequestID != observed || first.RequestIDQuality != RequestIDObserved {
		t.Fatalf("observed record = %+v", first)
	}
	if second.RequestIDQuality != RequestIDSynthetic || third.RequestIDQuality != RequestIDSynthetic ||
		!ValidProxyRequestID(second.ProxyRequestID) || !ValidProxyRequestID(third.ProxyRequestID) ||
		second.ProxyRequestID == third.ProxyRequestID {
		t.Fatalf("synthetic IDs = %q and %q", second.ProxyRequestID, third.ProxyRequestID)
	}
}

func TestManagerBoundsEndpointClassBeforeSanitizerTap(t *testing.T) {
	manager := NewManager(1)
	var captured string
	if _, errRegister := manager.RegisterSanitizerTapNamed("capture", pluginFunc(func(_ context.Context, record Record) {
		captured = record.EndpointClass
	})); errRegister != nil {
		t.Fatalf("register sanitizer tap: %v", errRegister)
	}
	ctx := WithEndpointClass(context.Background(), strings.Repeat("é", 200))
	manager.Publish(ctx, Record{})
	if len(captured) > 256 || !utf8.ValidString(captured) {
		t.Fatalf("endpoint class is %d bytes and valid=%t", len(captured), utf8.ValidString(captured))
	}
}

func TestManagerRegistrationFailureIsVisible(t *testing.T) {
	manager := NewManager(1)
	for index := 0; index < maxObserverLanes; index++ {
		if _, errRegister := manager.Register(pluginFunc(func(context.Context, Record) {})); errRegister != nil {
			t.Fatalf("registration %d: %v", index, errRegister)
		}
	}
	if _, errRegister := manager.Register(pluginFunc(func(context.Context, Record) {})); !errors.Is(errRegister, ErrObserverCapacity) {
		t.Fatalf("extra registration error = %v, want ErrObserverCapacity", errRegister)
	}
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if errClose := manager.Close(ctx); errClose != nil {
		t.Fatalf("close manager: %v", errClose)
	}
}

func waitChannel(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func errorsIsDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
