package usage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultObserverQueueCapacity is the total number of queued generic
	// observer deliveries across all lanes.
	DefaultObserverQueueCapacity = 4096
	// MaxObserverSnapshotBytes bounds one copied generic observer record.
	MaxObserverSnapshotBytes = 16 * 1024
	// DefaultObserverByteCapacity bounds queued records and lane buffers.
	DefaultObserverByteCapacity = 64 * 1024 * 1024
	// DefaultObserverDrainTimeout bounds legacy Stop calls.
	DefaultObserverDrainTimeout = 5 * time.Second
	maxObserverLanes            = 64
)

var (
	ErrManagerClosed       = errors.New("usage manager is stopped")
	ErrObserverCapacity    = errors.New("usage observer capacity exhausted")
	ErrInvalidRegistration = errors.New("invalid usage registration")
)

// ObserverContextSnapshotter copies allowlisted context values into a detached
// context and reports their bounded byte cost.
type ObserverContextSnapshotter func(context.Context, int) (context.Context, int)

var observerContextSnapshotter struct {
	sync.RWMutex
	fn ObserverContextSnapshotter
}

// SetObserverContextSnapshotter installs the process adapter used to detach
// internal request metadata before generic asynchronous delivery.
func SetObserverContextSnapshotter(snapshotter ObserverContextSnapshotter) {
	observerContextSnapshotter.Lock()
	observerContextSnapshotter.fn = snapshotter
	observerContextSnapshotter.Unlock()
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

// UnregisterFunc detaches one registration and waits for its active lane to drain.
type UnregisterFunc func(context.Context) error

// LaneStats describes one generic asynchronous observer lane.
type LaneStats struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	Capacity   int    `json:"capacity"`
	Depth      int    `json:"depth"`
	Delivered  uint64 `json:"delivered"`
	Dropped    uint64 `json:"dropped"`
	Panics     uint64 `json:"panics"`
	Registered bool   `json:"registered"`
}

// ManagerStats is a point-in-time delivery health snapshot.
type ManagerStats struct {
	QueueCapacity    int         `json:"queue_capacity"`
	QueueDepth       int         `json:"queue_depth"`
	ByteCapacity     int64       `json:"byte_capacity"`
	QueueBytes       int64       `json:"queue_bytes"`
	Published        uint64      `json:"published"`
	Dropped          uint64      `json:"dropped"`
	ClosedDrops      uint64      `json:"closed_drops"`
	AccountingCalls  uint64      `json:"accounting_calls"`
	AccountingPanic  uint64      `json:"accounting_panics"`
	TapCalls         uint64      `json:"tap_calls"`
	TapDropped       uint64      `json:"tap_dropped"`
	TapPanics        uint64      `json:"tap_panics"`
	ActivePublishers int         `json:"active_publishers"`
	Lanes            []LaneStats `json:"lanes"`
}

type managerState uint8

const (
	managerIdle managerState = iota
	managerRunning
	managerClosing
	managerStopped
)

type registration struct {
	name    string
	version uint64
	plugin  Plugin
}

type observerLane struct {
	id       uint64
	name     string
	version  uint64
	plugin   Plugin
	removed  bool
	reserved int64
	current  *laneRun

	delivered atomic.Uint64
	dropped   atomic.Uint64
	panics    atomic.Uint64
}

type laneRun struct {
	mu        sync.Mutex
	queue     chan *queueItem
	abort     chan struct{}
	done      chan struct{}
	accepting bool
	closeOnce sync.Once
	abortOnce sync.Once
	aborted   atomic.Bool
}

type queueItem struct {
	ctx    context.Context
	record Record
	plugin Plugin
	size   int64
}

type contextSnapshot struct {
	base           context.Context
	proxyRequestID string
	endpointClass  string
	alias          string
	reasoning      string
	serviceTier    string
	generate       bool
}

// Manager delivers trusted callbacks inline and generic observers through
// independent bounded lanes.
type Manager struct {
	mu sync.Mutex

	capacity         int
	byteCapacity     int64
	queueDepth       int
	queueBytes       int64
	reservedBytes    int64
	state            managerState
	generation       uint64
	closeDone        chan struct{}
	nextID           uint64
	activePublishers int
	publishersDone   chan struct{}

	lanes      map[uint64]*observerLane
	namedLanes map[string]*observerLane
	accounting map[string]registration
	taps       map[string]registration

	published       atomic.Uint64
	dropped         atomic.Uint64
	closedDrops     atomic.Uint64
	accountingCalls atomic.Uint64
	accountingPanic atomic.Uint64
	tapCalls        atomic.Uint64
	tapDropped      atomic.Uint64
	tapPanics       atomic.Uint64
}

// NewManager constructs a manager with a total generic observer queue bound.
func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = DefaultObserverQueueCapacity
	}
	return &Manager{
		capacity:     buffer,
		byteCapacity: DefaultObserverByteCapacity,
		state:        managerIdle,
		lanes:        make(map[uint64]*observerLane),
		namedLanes:   make(map[string]*observerLane),
		accounting:   make(map[string]registration),
		taps:         make(map[string]registration),
	}
}

// Start launches all registered generic observer lanes. A stopped manager can
// be started again with the same registrations.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.state == managerRunning || m.state == managerClosing {
		m.mu.Unlock()
		return
	}
	m.startLocked()
	generation := m.generation
	m.mu.Unlock()

	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			m.closeGeneration(generation)
		}()
	}
}

func (m *Manager) startLocked() {
	m.generation++
	m.state = managerRunning
	m.closeDone = make(chan struct{})
	for _, lane := range m.lanes {
		m.startLaneLocked(lane)
	}
}

func (m *Manager) startLaneLocked(lane *observerLane) {
	if lane == nil || lane.removed {
		return
	}
	run := &laneRun{
		queue:     make(chan *queueItem, m.capacity),
		abort:     make(chan struct{}),
		done:      make(chan struct{}),
		accepting: true,
	}
	lane.current = run
	go m.runLane(lane, run)
}

func (m *Manager) closeGeneration(generation uint64) {
	m.mu.Lock()
	if m.state != managerRunning || m.generation != generation {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultObserverDrainTimeout)
	defer cancel()
	_ = m.Close(ctx)
}

// Close detaches publishers, drains each generic lane independently, and stops
// waiting when ctx expires.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	switch m.state {
	case managerIdle, managerStopped:
		m.mu.Unlock()
		return nil
	case managerClosing:
		done := m.closeDone
		m.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.state = managerClosing
	generation := m.generation
	done := m.closeDone
	publishersDone := m.publishersDone
	runs := make([]*laneRun, 0, len(m.lanes))
	for _, lane := range m.lanes {
		if lane.current != nil {
			runs = append(runs, lane.current)
			lane.current = nil
		}
	}
	m.mu.Unlock()

	errClose := waitPublishers(ctx, publishersDone)

	for _, run := range runs {
		closeLaneRun(run)
	}

	if errClose == nil {
		errClose = waitLaneRuns(ctx, runs)
	}
	if errClose != nil {
		for _, run := range runs {
			m.abandonLaneRun(run)
		}
	}

	m.mu.Lock()
	if m.generation == generation && m.state == managerClosing {
		m.state = managerStopped
		close(done)
	}
	m.mu.Unlock()
	return errClose
}

func waitPublishers(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop drains with the documented default deadline. Use Close when the caller
// already owns a shutdown deadline.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultObserverDrainTimeout)
	defer cancel()
	if errClose := m.Close(ctx); errClose != nil {
		log.WithError(errClose).Warn("usage: observer drain deadline expired")
	}
}

func closeLaneRun(run *laneRun) {
	if run == nil {
		return
	}
	run.closeOnce.Do(func() {
		run.mu.Lock()
		run.accepting = false
		close(run.queue)
		run.mu.Unlock()
	})
}

func waitLaneRuns(ctx context.Context, runs []*laneRun) error {
	for _, run := range runs {
		if run == nil {
			continue
		}
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *Manager) abandonLaneRun(run *laneRun) {
	if run == nil {
		return
	}
	run.abortOnce.Do(func() {
		run.aborted.Store(true)
		close(run.abort)
	})
	for item := range run.queue {
		if item == nil {
			continue
		}
		m.releaseQueue(item.size)
		m.dropped.Add(1)
	}
}

// Register appends an anonymous generic observer lane.
func (m *Manager) Register(plugin Plugin) (UnregisterFunc, error) {
	return m.registerObserver("", plugin)
}

// RegisterNamed registers or replaces a generic observer lane by name.
func (m *Manager) RegisterNamed(name string, plugin Plugin) (UnregisterFunc, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: observer name is empty", ErrInvalidRegistration)
	}
	return m.registerObserver(name, plugin)
}

func (m *Manager) registerObserver(name string, plugin Plugin) (UnregisterFunc, error) {
	if m == nil || plugin == nil {
		return nil, fmt.Errorf("%w: nil manager or plugin", ErrInvalidRegistration)
	}

	m.mu.Lock()
	if name != "" {
		if lane := m.namedLanes[name]; lane != nil && !lane.removed {
			lane.version++
			lane.plugin = plugin
			version := lane.version
			m.mu.Unlock()
			return func(ctx context.Context) error { return m.unregisterLane(ctx, lane.id, version) }, nil
		}
	}
	if len(m.lanes) >= maxObserverLanes {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: maximum %d lanes", ErrObserverCapacity, maxObserverLanes)
	}
	reserved := int64(m.capacity) * int64(unsafe.Sizeof((*queueItem)(nil)))
	if m.reservedBytes+reserved >= m.byteCapacity {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: lane buffer exceeds byte budget", ErrObserverCapacity)
	}
	m.nextID++
	lane := &observerLane{id: m.nextID, name: name, version: 1, plugin: plugin, reserved: reserved}
	m.lanes[lane.id] = lane
	if name != "" {
		m.namedLanes[name] = lane
	}
	m.reservedBytes += reserved
	if m.state == managerRunning {
		m.startLaneLocked(lane)
	}
	version := lane.version
	m.mu.Unlock()
	return func(ctx context.Context) error { return m.unregisterLane(ctx, lane.id, version) }, nil
}

// UnregisterNamed detaches a named generic observer.
func (m *Manager) UnregisterNamed(ctx context.Context, name string) error {
	if m == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	m.mu.Lock()
	lane := m.namedLanes[name]
	if lane == nil {
		m.mu.Unlock()
		return nil
	}
	id, version := lane.id, lane.version
	m.mu.Unlock()
	return m.unregisterLane(ctx, id, version)
}

func (m *Manager) unregisterLane(ctx context.Context, id, version uint64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	lane := m.lanes[id]
	if lane == nil || lane.removed || lane.version != version {
		m.mu.Unlock()
		return nil
	}
	lane.removed = true
	delete(m.lanes, id)
	if lane.name != "" && m.namedLanes[lane.name] == lane {
		delete(m.namedLanes, lane.name)
	}
	run := lane.current
	lane.current = nil
	publishersDone := m.publishersDone
	m.mu.Unlock()

	if errWait := waitPublishers(ctx, publishersDone); errWait != nil {
		if run != nil {
			closeLaneRun(run)
			m.abandonLaneRun(run)
			go m.releaseLaneWhenDone(lane, run)
		} else {
			m.releaseLaneReservation(lane)
		}
		return errWait
	}
	if run != nil {
		closeLaneRun(run)
		select {
		case <-run.done:
		case <-ctx.Done():
			m.abandonLaneRun(run)
			go m.releaseLaneWhenDone(lane, run)
			return ctx.Err()
		}
	}
	m.releaseLaneReservation(lane)
	return nil
}

func (m *Manager) releaseLaneWhenDone(lane *observerLane, run *laneRun) {
	<-run.done
	m.releaseLaneReservation(lane)
}

func (m *Manager) releaseLaneReservation(lane *observerLane) {
	if lane == nil {
		return
	}
	m.mu.Lock()
	if lane.reserved > 0 {
		m.reservedBytes -= lane.reserved
		lane.reserved = 0
	}
	m.mu.Unlock()
}

// RegisterAccountingNamed installs a trusted synchronous accounting callback.
// Accounting callbacks run before sanitizer taps and generic observers.
func (m *Manager) RegisterAccountingNamed(name string, plugin Plugin) (UnregisterFunc, error) {
	return m.registerInline(name, plugin, false)
}

// RegisterSanitizerTapNamed installs a trusted synchronous sanitizer tap. The
// tap must only copy bounded fields and attempt its own nonblocking enqueue.
func (m *Manager) RegisterSanitizerTapNamed(name string, plugin Plugin) (UnregisterFunc, error) {
	return m.registerInline(name, plugin, true)
}

func (m *Manager) registerInline(name string, plugin Plugin, tap bool) (UnregisterFunc, error) {
	if m == nil || plugin == nil {
		return nil, fmt.Errorf("%w: nil manager or plugin", ErrInvalidRegistration)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: callback name is empty", ErrInvalidRegistration)
	}
	m.mu.Lock()
	m.nextID++
	entry := registration{name: name, version: m.nextID, plugin: plugin}
	target := m.accounting
	if tap {
		target = m.taps
	}
	target[name] = entry
	m.mu.Unlock()
	return func(ctx context.Context) error {
		if ctx == nil {
			ctx = context.Background()
		}
		m.mu.Lock()
		current, ok := target[name]
		if ok && current.version == entry.version {
			delete(target, name)
		}
		publishersDone := m.publishersDone
		m.mu.Unlock()
		return waitPublishers(ctx, publishersDone)
	}, nil
}

// Publish runs accounting and sanitizer taps inline, then attempts one
// nonblocking enqueue per generic observer lane.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record = normalizeRecordRequestID(ctx, record)
	record.EndpointClass = strings.TrimSpace(record.EndpointClass)
	if record.EndpointClass == "" {
		record.EndpointClass = EndpointClassFromContext(ctx)
	}
	record.EndpointClass = truncateUTF8(record.EndpointClass, 256)

	m.mu.Lock()
	if m.state == managerIdle {
		m.startLocked()
	}
	if m.state != managerRunning {
		m.closedDrops.Add(1)
		m.mu.Unlock()
		return
	}
	m.activePublishers++
	if m.activePublishers == 1 {
		m.publishersDone = make(chan struct{})
	}
	accounting := copyRegistrations(m.accounting)
	taps := copyRegistrations(m.taps)
	deliveries := make([]struct {
		lane   *observerLane
		run    *laneRun
		plugin Plugin
	}, 0, len(m.lanes))
	for _, lane := range m.lanes {
		if lane.removed || lane.current == nil || lane.plugin == nil {
			continue
		}
		deliveries = append(deliveries, struct {
			lane   *observerLane
			run    *laneRun
			plugin Plugin
		}{lane: lane, run: lane.current, plugin: lane.plugin})
	}
	m.mu.Unlock()
	defer m.finishPublish()

	m.published.Add(1)
	for _, entry := range accounting {
		m.accountingCalls.Add(1)
		if !invokeInline(entry.plugin, ctx, record) {
			m.accountingPanic.Add(1)
		}
	}
	for _, entry := range taps {
		m.tapCalls.Add(1)
		if !invokeInline(entry.plugin, ctx, record) {
			m.tapPanics.Add(1)
			m.tapDropped.Add(1)
		}
	}
	if len(deliveries) == 0 {
		return
	}

	frozenRecord, frozenContext, size := freezeObserverSnapshot(ctx, record)
	for _, delivery := range deliveries {
		item := queueItem{
			ctx:    frozenContext.context(),
			record: cloneRecord(frozenRecord),
			plugin: delivery.plugin,
			size:   size,
		}
		reserved := m.reserveQueue(size)
		if !reserved || !enqueueLane(delivery.run, item) {
			if reserved {
				m.releaseQueue(size)
			}
			delivery.lane.dropped.Add(1)
			m.dropped.Add(1)
		}
	}
}

func (m *Manager) finishPublish() {
	m.mu.Lock()
	if m.activePublishers > 0 {
		m.activePublishers--
		if m.activePublishers == 0 && m.publishersDone != nil {
			close(m.publishersDone)
			m.publishersDone = nil
		}
	}
	m.mu.Unlock()
}

func copyRegistrations(source map[string]registration) []registration {
	result := make([]registration, 0, len(source))
	for _, entry := range source {
		result = append(result, entry)
	}
	return result
}

func invokeInline(plugin Plugin, ctx context.Context, record Record) (ok bool) {
	if plugin == nil {
		return true
	}
	ok = true
	defer func() {
		if recovered := recover(); recovered != nil {
			ok = false
			log.WithField("plugin_type", fmt.Sprintf("%T", plugin)).Error("usage: trusted callback panic recovered")
		}
	}()
	plugin.HandleUsage(ctx, record)
	return ok
}

func (m *Manager) runLane(lane *observerLane, run *laneRun) {
	defer close(run.done)
	for {
		select {
		case <-run.abort:
			return
		default:
		}
		select {
		case <-run.abort:
			return
		case item, ok := <-run.queue:
			if !ok {
				return
			}
			if item == nil {
				continue
			}
			m.releaseQueue(item.size)
			if run.aborted.Load() {
				lane.dropped.Add(1)
				m.dropped.Add(1)
				return
			}
			if invokeObserver(item.plugin, item.ctx, item.record) {
				lane.delivered.Add(1)
			} else {
				lane.panics.Add(1)
			}
		}
	}
}

func invokeObserver(plugin Plugin, ctx context.Context, record Record) (ok bool) {
	if plugin == nil {
		return true
	}
	ok = true
	defer func() {
		if recovered := recover(); recovered != nil {
			ok = false
			log.WithField("plugin_type", fmt.Sprintf("%T", plugin)).Error("usage: observer panic recovered")
		}
	}()
	plugin.HandleUsage(ctx, record)
	return ok
}

func enqueueLane(run *laneRun, item queueItem) bool {
	if run == nil {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.accepting {
		return false
	}
	select {
	case run.queue <- &item:
		return true
	default:
		return false
	}
}

func (m *Manager) reserveQueue(size int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != managerRunning || m.queueDepth >= m.capacity || m.queueBytes+size+m.reservedBytes > m.byteCapacity {
		return false
	}
	m.queueDepth++
	m.queueBytes += size
	return true
}

func (m *Manager) releaseQueue(size int64) {
	m.mu.Lock()
	if m.queueDepth > 0 {
		m.queueDepth--
	}
	if size >= m.queueBytes {
		m.queueBytes = 0
	} else {
		m.queueBytes -= size
	}
	m.mu.Unlock()
}

// Stats returns queue, loss, and panic counters without exposing record data.
func (m *Manager) Stats() ManagerStats {
	if m == nil {
		return ManagerStats{}
	}
	m.mu.Lock()
	stats := ManagerStats{
		QueueCapacity:    m.capacity,
		QueueDepth:       m.queueDepth,
		ByteCapacity:     m.byteCapacity,
		QueueBytes:       m.queueBytes,
		Published:        m.published.Load(),
		Dropped:          m.dropped.Load(),
		ClosedDrops:      m.closedDrops.Load(),
		AccountingCalls:  m.accountingCalls.Load(),
		AccountingPanic:  m.accountingPanic.Load(),
		TapCalls:         m.tapCalls.Load(),
		TapDropped:       m.tapDropped.Load(),
		TapPanics:        m.tapPanics.Load(),
		ActivePublishers: m.activePublishers,
		Lanes:            make([]LaneStats, 0, len(m.lanes)),
	}
	for _, lane := range m.lanes {
		depth := 0
		if lane.current != nil {
			depth = len(lane.current.queue)
		}
		stats.Lanes = append(stats.Lanes, LaneStats{
			ID:         lane.id,
			Name:       lane.name,
			Capacity:   m.capacity,
			Depth:      depth,
			Delivered:  lane.delivered.Load(),
			Dropped:    lane.dropped.Load(),
			Panics:     lane.panics.Load(),
			Registered: !lane.removed,
		})
	}
	sort.Slice(stats.Lanes, func(left, right int) bool {
		return stats.Lanes[left].ID < stats.Lanes[right].ID
	})
	m.mu.Unlock()
	return stats
}

func freezeObserverSnapshot(ctx context.Context, record Record) (Record, contextSnapshot, int64) {
	remaining := MaxObserverSnapshotBytes - 512
	copyString := func(value string) string {
		value = truncateUTF8(value, remaining)
		remaining -= len(value)
		return value
	}

	record.ProxyRequestID = copyString(record.ProxyRequestID)
	record.Provider = copyString(record.Provider)
	record.ExecutorType = copyString(record.ExecutorType)
	record.Model = copyString(record.Model)
	record.Alias = copyString(record.Alias)
	record.APIKey = copyString(record.APIKey)
	record.AuthID = copyString(record.AuthID)
	record.AuthIndex = copyString(record.AuthIndex)
	record.AccessTokenSHA256 = copyString(record.AccessTokenSHA256)
	record.AuthType = copyString(record.AuthType)
	record.Source = copyString(record.Source)
	record.EndpointClass = copyString(record.EndpointClass)
	record.ReasoningEffort = copyString(record.ReasoningEffort)
	record.ServiceTier = copyString(record.ServiceTier)
	record.RequestServiceTier = copyString(record.RequestServiceTier)
	record.ResponseServiceTier = copyString(record.ResponseServiceTier)
	record.Fail.Body = copyString(record.Fail.Body)
	record.Detail.ResponseServiceTier = copyString(record.Detail.ResponseServiceTier)
	record.ResponseHeaders = copyHeaders(record.ResponseHeaders, &remaining)
	if record.Generate != nil {
		generate := *record.Generate
		record.Generate = &generate
	}

	snapshot := contextSnapshot{
		proxyRequestID: record.ProxyRequestID,
		endpointClass:  copyString(EndpointClassFromContext(ctx)),
		alias:          copyString(RequestedModelAliasFromContext(ctx)),
		reasoning:      copyString(ReasoningEffortFromContext(ctx)),
		serviceTier:    copyString(ServiceTierFromContext(ctx)),
		generate:       GenerateFromContext(ctx),
	}
	snapshot.base, _ = snapshotObserverContext(ctx, &remaining)
	return record, snapshot, int64(MaxObserverSnapshotBytes - remaining)
}

func snapshotObserverContext(ctx context.Context, remaining *int) (context.Context, int) {
	if remaining == nil || *remaining <= 0 {
		return context.Background(), 0
	}
	observerContextSnapshotter.RLock()
	snapshotter := observerContextSnapshotter.fn
	observerContextSnapshotter.RUnlock()
	if snapshotter == nil {
		return context.Background(), 0
	}
	base, used := snapshotter(ctx, *remaining)
	if base == nil {
		base = context.Background()
	}
	if used < 0 {
		used = 0
	}
	if used > *remaining {
		used = *remaining
	}
	*remaining -= used
	return base, used
}

func (s contextSnapshot) context() context.Context {
	ctx := s.base
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = WithProxyRequestID(ctx, s.proxyRequestID)
	ctx = WithEndpointClass(ctx, s.endpointClass)
	ctx = WithRequestedModelAlias(ctx, s.alias)
	ctx = WithReasoningEffort(ctx, s.reasoning)
	ctx = WithServiceTier(ctx, s.serviceTier)
	ctx = WithGenerate(ctx, s.generate)
	return ctx
}

func cloneRecord(record Record) Record {
	record.ResponseHeaders = record.ResponseHeaders.Clone()
	if record.Generate != nil {
		generate := *record.Generate
		record.Generate = &generate
	}
	return record
}

func copyHeaders(source http.Header, remaining *int) http.Header {
	if len(source) == 0 || remaining == nil || *remaining <= 0 {
		return nil
	}
	result := make(http.Header)
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 64 {
		keys = keys[:64]
	}
	for _, sourceKey := range keys {
		if *remaining <= 64 {
			break
		}
		*remaining -= 64
		key := sourceKey
		values := source[sourceKey]
		key = truncateUTF8(key, *remaining)
		*remaining -= len(key)
		if key == "" || *remaining <= 0 {
			break
		}
		copied := make([]string, 0, len(values))
		if len(values) > 16 {
			values = values[:16]
		}
		for _, value := range values {
			if *remaining <= 16 {
				break
			}
			*remaining -= 16
			value = truncateUTF8(value, *remaining)
			*remaining -= len(value)
			copied = append(copied, value)
			if *remaining <= 0 {
				break
			}
		}
		result[key] = copied
		if *remaining <= 0 {
			break
		}
	}
	return result
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

var defaultManager = NewManager(DefaultObserverQueueCapacity)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a generic observer on the default manager.
func RegisterPlugin(plugin Plugin) (UnregisterFunc, error) { return DefaultManager().Register(plugin) }

// RegisterNamedPlugin registers or replaces a named generic observer.
func RegisterNamedPlugin(name string, plugin Plugin) (UnregisterFunc, error) {
	return DefaultManager().RegisterNamed(name, plugin)
}

// RegisterAccountingNamedPlugin registers trusted synchronous accounting.
func RegisterAccountingNamedPlugin(name string, plugin Plugin) (UnregisterFunc, error) {
	return DefaultManager().RegisterAccountingNamed(name, plugin)
}

// RegisterSanitizerTapNamedPlugin registers a trusted synchronous sanitizer tap.
func RegisterSanitizerTapNamedPlugin(name string, plugin Plugin) (UnregisterFunc, error) {
	return DefaultManager().RegisterSanitizerTapNamed(name, plugin)
}

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// StartDefault starts or restarts the default manager.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// CloseDefault drains the default manager within ctx.
func CloseDefault(ctx context.Context) error { return DefaultManager().Close(ctx) }

// StopDefault stops the default manager with the default drain deadline.
func StopDefault() { DefaultManager().Stop() }
