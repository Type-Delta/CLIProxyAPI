package collector

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	log "github.com/sirupsen/logrus"
)

const restartWindow = 5 * time.Minute

type Callbacks struct {
	Queue      func(depth, dropped int64)
	State      func(model.AnalyticsState, string)
	Wrote      func(time.Time)
	Panic      func(category string, at time.Time, restartCount int)
	Abandoned  func(int64)
	Generation func(uint64)
}

type Options struct {
	Capacity         int
	BatchSize        int
	FlushInterval    time.Duration
	FailureThreshold int
	RestartDelays    []time.Duration
	Now              func() time.Time
	Callbacks        Callbacks
}

// queuedEvent carries either an event insert or a proxy response patch. Both
// share one FIFO queue so a patch can never overtake the event it completes.
type queuedEvent struct {
	event model.Event
	patch *ProxyResponsePatch
}

// batch keeps inserts and patches in arrival order until a flush.
type batch struct {
	events  []model.Event
	patches []ProxyResponsePatch
}

func (b *batch) add(item queuedEvent) {
	if item.patch != nil {
		b.patches = append(b.patches, *item.patch)
		return
	}
	b.events = append(b.events, item.event)
}

func (b *batch) size() int { return len(b.events) + len(b.patches) }

func (b *batch) reset() { b.events = b.events[:0]; b.patches = b.patches[:0] }

type Stats struct {
	Capacity        int64
	Depth           int64
	Dropped         int64
	Rejected        int64
	TruncatedFields int64
	InFlight        int64
}

// Collector owns a bounded queue of sanitized events. Enqueue never waits.
type Collector struct {
	writer Writer
	queue  chan queuedEvent
	stop   chan struct{}
	done   chan struct{}
	retry  chan struct{}

	workerCtx    context.Context
	cancelWorker context.CancelFunc

	generation    atomic.Uint64
	accepting     atomic.Bool
	closed        atomic.Bool
	closeTimedOut atomic.Bool
	dropped       atomic.Int64
	rejected      atomic.Int64
	truncated     atomic.Int64
	inFlight      atomic.Int64
	retryable     atomic.Bool

	settingsMu       sync.RWMutex
	batchSize        int
	flushInterval    time.Duration
	failureThreshold int

	restartDelays []time.Duration
	now           func() time.Time
	callbacks     Callbacks
	closeOnce     sync.Once
	restartMu     sync.Mutex
	restarts      []time.Time
	logMu         sync.Mutex
	lastLog       map[string]time.Time
}

func New(writer Writer, options Options) (*Collector, error) {
	if writer == nil {
		return nil, fmt.Errorf("collector writer is required")
	}
	if options.Capacity < 1 || options.BatchSize < 1 || options.BatchSize > options.Capacity {
		return nil, fmt.Errorf("invalid collector queue or batch size")
	}
	if options.FlushInterval <= 0 || options.FailureThreshold < 1 {
		return nil, fmt.Errorf("invalid collector interval or failure threshold")
	}
	if len(options.RestartDelays) == 0 {
		options.RestartDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	c := &Collector{
		writer:           writer,
		queue:            make(chan queuedEvent, options.Capacity),
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
		retry:            make(chan struct{}, 1),
		batchSize:        options.BatchSize,
		flushInterval:    options.FlushInterval,
		failureThreshold: options.FailureThreshold,
		restartDelays:    append([]time.Duration(nil), options.RestartDelays...),
		now:              options.Now,
		callbacks:        options.Callbacks,
		lastLog:          make(map[string]time.Time),
	}
	c.workerCtx, c.cancelWorker = context.WithCancel(context.Background())
	c.generation.Store(1)
	return c, nil
}

func (c *Collector) Start() {
	if c == nil || c.closed.Load() || c.accepting.Swap(true) {
		return
	}
	c.safeGeneration(c.generation.Load())
	go c.supervise()
}

func (c *Collector) Generation() uint64 {
	if c == nil {
		return 0
	}
	return c.generation.Load()
}

func (c *Collector) Enqueue(generation uint64, event Event) bool {
	if c == nil || c.closed.Load() || !c.accepting.Load() || generation != c.generation.Load() {
		c.drop()
		return false
	}
	select {
	case c.queue <- queuedEvent{event: event}:
		return true
	default:
		c.drop()
		return false
	}
}

// EnqueueProxyResponse queues the downstream outcome of one proxy request. It
// is lossy under the same rules as Enqueue.
func (c *Collector) EnqueueProxyResponse(generation uint64, patch ProxyResponsePatch) bool {
	if c == nil || c.closed.Load() || !c.accepting.Load() || generation != c.generation.Load() {
		c.drop()
		return false
	}
	select {
	case c.queue <- queuedEvent{patch: &patch}:
		return true
	default:
		c.drop()
		return false
	}
}

func (c *Collector) Rejected() {
	if c == nil {
		return
	}
	c.rejected.Add(1)
}

func (c *Collector) Truncated(count int64) {
	if c == nil || count <= 0 {
		return
	}
	c.truncated.Add(count)
}

func (c *Collector) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Capacity:        int64(cap(c.queue)),
		Depth:           int64(len(c.queue)),
		Dropped:         c.dropped.Load(),
		Rejected:        c.rejected.Load(),
		TruncatedFields: c.truncated.Load(),
		InFlight:        c.inFlight.Load(),
	}
}

func (c *Collector) Reconfigure(batchSize int, flushInterval time.Duration, failureThreshold int) error {
	if c == nil || batchSize < 1 || batchSize > cap(c.queue) || flushInterval <= 0 || failureThreshold < 1 {
		return fmt.Errorf("invalid collector reconfiguration")
	}
	c.settingsMu.Lock()
	c.batchSize = batchSize
	c.flushInterval = flushInterval
	c.failureThreshold = failureThreshold
	c.settingsMu.Unlock()
	return nil
}

func (c *Collector) Retry() bool {
	if c == nil || c.closed.Load() || c.accepting.Load() || !c.retryable.Load() {
		return false
	}
	c.restartMu.Lock()
	c.restarts = nil
	c.restartMu.Unlock()
	select {
	case c.retry <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *Collector) Detach() {
	if c == nil {
		return
	}
	c.accepting.Store(false)
	c.generation.Add(1)
}

// Drain waits until the detached generation has no queued or in-flight events.
// It does not stop the worker or close the writer.
func (c *Collector) Drain(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		stats := c.Stats()
		if stats.Depth == 0 && stats.InFlight == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Resume installs a fresh intake generation after an exclusive maintenance
// operation. Events tied to the detached generation remain unable to enqueue.
func (c *Collector) Resume() bool {
	if c == nil || c.closed.Load() || c.accepting.Swap(true) {
		return false
	}
	generation := c.generation.Add(1)
	c.safeGeneration(generation)
	return true
}

// Close stops intake and asks the worker to drain queued events. A failed batch
// interrupted by shutdown is reported as abandoned. If ctx expires first,
// queued and in-flight events are reported as abandoned before the worker is
// canceled.
func (c *Collector) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.Detach()
		close(c.stop)
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-c.done:
		c.cancelWorker()
		return nil
	case <-ctx.Done():
		c.closeTimedOut.Store(true)
		abandoned := int64(len(c.queue)) + c.inFlight.Load()
		c.cancelWorker()
		c.safeCall(func() {
			if c.callbacks.Abandoned != nil {
				c.callbacks.Abandoned(abandoned)
			}
		})
		return ctx.Err()
	}
}

func (c *Collector) supervise() {
	defer close(c.done)
	for {
		panicked := c.runWorkerProtected()
		if !panicked || c.closed.Load() {
			return
		}
		c.Detach()
		now := c.now().UTC()
		count := c.recordRestart(now)
		c.safeCall(func() {
			if c.callbacks.Panic != nil {
				c.callbacks.Panic("worker_panic", now, count)
			}
		})
		if count > len(c.restartDelays) {
			c.retryable.Store(true)
			c.safeState(model.StateCircuitOpen, "worker_panic_loop")
			select {
			case <-c.retry:
				c.retryable.Store(false)
				c.attach()
				c.safeState(model.StateDegraded, "worker_restart")
				continue
			case <-c.stop:
				return
			}
		}
		c.safeState(model.StateDegraded, "worker_panic")
		timer := time.NewTimer(c.restartDelays[count-1])
		select {
		case <-timer.C:
			c.attach()
		case <-c.stop:
			stopAndDrainTimer(timer)
			return
		}
	}
}

func (c *Collector) runWorkerProtected() (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	c.runWorker()
	return false
}

func (c *Collector) runWorker() {
	pending := &batch{events: make([]model.Event, 0, c.currentBatchSize())}
	timer := time.NewTimer(c.currentFlushInterval())
	defer timer.Stop()
	circuitState := newCircuit(c.currentThreshold())

	for {
		select {
		case item := <-c.queue:
			pending.add(item)
			if pending.size() >= c.currentBatchSize() {
				c.write(pending, &circuitState)
			}
			c.safeQueue()
		case <-timer.C:
			c.write(pending, &circuitState)
			timer.Reset(c.currentFlushInterval())
		case <-c.stop:
			for {
				select {
				case item := <-c.queue:
					pending.add(item)
					if pending.size() >= c.currentBatchSize() {
						c.write(pending, &circuitState)
					}
				default:
					c.write(pending, &circuitState)
					return
				}
			}
		}
	}
}

func (c *Collector) write(pending *batch, state *circuit) {
	if pending.size() == 0 {
		pending.reset()
		return
	}
	c.inFlight.Store(int64(pending.size()))
	defer func() {
		if value := recover(); value != nil {
			c.dropped.Add(int64(pending.size()))
			c.safeQueue()
			c.inFlight.Store(0)
			panic(value)
		}
		c.inFlight.Store(0)
	}()
	for {
		if state.open && state.permanent {
			c.retryable.Store(true)
			select {
			case <-c.retry:
				c.retryable.Store(false)
				state.succeeded()
			case <-c.stop:
				c.abandon(pending)
				return
			}
		}
		err := c.writeBatch(pending)
		if err == nil {
			state.succeeded()
			now := c.now().UTC()
			c.safeCall(func() {
				if c.callbacks.Wrote != nil {
					c.callbacks.Wrote(now)
				}
			})
			if !c.closed.Load() && !c.accepting.Load() {
				c.attach()
			}
			c.safeState(model.StateReady, "")
			pending.reset()
			return
		}
		permanent, category := classifyWriteError(err)
		opened := state.failed(permanent)
		c.safeState(model.StateDegraded, category)
		if opened {
			c.Detach()
			c.safeState(model.StateCircuitOpen, category)
		}
		if permanent {
			continue
		}
		timer := time.NewTimer(state.nextBackoff())
		select {
		case <-timer.C:
			continue
		case <-c.stop:
			stopAndDrainTimer(timer)
			c.abandon(pending)
			return
		}
	}
}

// writeBatch hands inserts and patches to the writer; writers that cannot
// patch only receive the events and the patches are dropped.
func (c *Collector) writeBatch(pending *batch) error {
	if patcher, ok := c.writer.(PatchWriter); ok {
		return patcher.WriteBatchWithPatches(c.workerCtx, pending.events, pending.patches)
	}
	if len(pending.patches) > 0 {
		c.dropped.Add(int64(len(pending.patches)))
	}
	if len(pending.events) == 0 {
		return nil
	}
	return c.writer.WriteBatch(c.workerCtx, pending.events)
}

func (c *Collector) attach() {
	if c.closed.Load() {
		return
	}
	generation := c.generation.Add(1)
	c.retryable.Store(false)
	c.accepting.Store(true)
	c.safeGeneration(generation)
}

func (c *Collector) recordRestart(now time.Time) int {
	c.restartMu.Lock()
	defer c.restartMu.Unlock()
	cutoff := now.Add(-restartWindow)
	kept := c.restarts[:0]
	for _, value := range c.restarts {
		if !value.Before(cutoff) {
			kept = append(kept, value)
		}
	}
	c.restarts = append(kept, now)
	return len(c.restarts)
}

func (c *Collector) drop() {
	c.dropped.Add(1)
}

func (c *Collector) abandon(pending *batch) {
	count := int64(pending.size())
	pending.reset()
	if count == 0 || c.closeTimedOut.Load() {
		return
	}
	c.safeCall(func() {
		if c.callbacks.Abandoned != nil {
			c.callbacks.Abandoned(count)
		}
	})
}

func (c *Collector) safeQueue() {
	c.safeCall(func() {
		if c.callbacks.Queue != nil {
			c.callbacks.Queue(int64(len(c.queue)), c.dropped.Load())
		}
	})
}

func (c *Collector) safeState(state model.AnalyticsState, category string) {
	category = safeCategory(category)
	c.logTransition(state, category)
	c.safeCall(func() {
		if c.callbacks.State != nil {
			c.callbacks.State(state, category)
		}
	})
}

func (c *Collector) logTransition(state model.AnalyticsState, category string) {
	category = safeCategory(category)
	key := string(state) + ":" + category
	now := c.now().UTC()
	c.logMu.Lock()
	last := c.lastLog[key]
	if state != model.StateCircuitOpen && now.Sub(last) < 10*time.Second {
		c.logMu.Unlock()
		return
	}
	c.lastLog[key] = now
	c.logMu.Unlock()
	entry := log.WithFields(log.Fields{
		"component": "cpauk_collector",
		"state":     state,
		"category":  category,
	})
	if state == model.StateDegraded || state == model.StateCircuitOpen {
		entry.Warn("analytics collector state changed")
		return
	}
	entry.Debug("analytics collector state changed")
}

func (c *Collector) safeGeneration(generation uint64) {
	c.safeCall(func() {
		if c.callbacks.Generation != nil {
			c.callbacks.Generation(generation)
		}
	})
}

func (c *Collector) safeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *Collector) currentBatchSize() int {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.batchSize
}

func (c *Collector) currentFlushInterval() time.Duration {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.flushInterval
}

func (c *Collector) currentThreshold() int {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.failureThreshold
}
