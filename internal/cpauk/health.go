package cpauk

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type Health = model.Health

type snapshots struct {
	cfg Config

	mu     sync.Mutex
	health model.Health
	state  atomic.Pointer[snapshotValue]
}

type snapshotValue struct {
	health       model.Health
	capabilities model.Capabilities
}

func newSnapshots(cfg Config, state State, category, message string) *snapshots {
	s := &snapshots{cfg: cfg}
	queueCapacity := int64(cfg.QueueCapacity)
	if queueCapacity < 0 {
		queueCapacity = 0
	}
	s.health = model.Health{
		State:                state,
		Category:             category,
		Message:              message,
		RestartWindowSeconds: int((5 * time.Minute) / time.Second),
		Queue: model.QueueSnapshot{
			Capacity: queueCapacity,
			MaxBytes: MaxQueueBytes,
		},
	}
	s.publishLocked()
	return s
}

func (s *snapshots) load() snapshotValue {
	if s == nil {
		return snapshotValue{}
	}
	value := s.state.Load()
	if value == nil {
		return snapshotValue{}
	}
	return cloneSnapshot(*value)
}

func (s *snapshots) mutate(fn func(*model.Health)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	fn(&s.health)
	s.publishLocked()
	s.mu.Unlock()
}

func (s *snapshots) setConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.health.Queue.Capacity = int64(cfg.QueueCapacity)
	s.publishLocked()
	s.mu.Unlock()
}

func (s *snapshots) publishLocked() {
	value := snapshotValue{
		health:       cloneHealth(s.health),
		capabilities: capabilitiesFor(s.health.State, s.cfg, s.health.Queue, s.health.LastSuccessfulWrite),
	}
	s.state.Store(&value)
}

func cloneSnapshot(value snapshotValue) snapshotValue {
	value.health = cloneHealth(value.health)
	value.capabilities.APISchemaVersions = append([]int(nil), value.capabilities.APISchemaVersions...)
	value.capabilities.LastSuccessfulWrite = cloneTime(value.capabilities.LastSuccessfulWrite)
	return value
}

func cloneHealth(value model.Health) model.Health {
	value.LastSuccessfulWrite = cloneTime(value.LastSuccessfulWrite)
	value.LastPanicAt = cloneTime(value.LastPanicAt)
	value.RetentionCutoff = cloneTime(value.RetentionCutoff)
	if value.ZoneMismatch != nil {
		mismatch := *value.ZoneMismatch
		value.ZoneMismatch = &mismatch
	}
	return value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
