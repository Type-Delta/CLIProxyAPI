package maintenance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

var (
	ErrJobNotFound      = errors.New("analytics maintenance job was not found")
	ErrJobRunning       = errors.New("another analytics maintenance job is running")
	ErrControllerClosed = errors.New("analytics maintenance controller is closed")
)

type Request struct {
	Kind    string
	Options map[string]any
}

// Hooks isolate the controller from CPA lifecycle types. Detach must stop new
// intake, Drain must finish the old generation, and Attach publishes a new
// generation after the operation passes its integrity check.
type Hooks struct {
	Detach func(context.Context) error
	Drain  func(context.Context) error
	Attach func(context.Context) error
}

type ProgressFunc func(percent int, checkpoint string)
type Operation func(context.Context, map[string]any, ProgressFunc) (map[string]any, error)

type Controller struct {
	mu         sync.RWMutex
	jobs       map[string]*job
	operations map[string]Operation
	hooks      Hooks
	now        func() time.Time
	closed     bool
}

type job struct {
	status model.JobStatus
	cancel context.CancelFunc
	done   chan struct{}
}

func New(hooks Hooks, operations map[string]Operation) *Controller {
	return &Controller{jobs: map[string]*job{}, operations: maps.Clone(operations), hooks: hooks, now: time.Now}
}

func (c *Controller) Start(_ context.Context, request Request) (model.JobStatus, error) {
	operation, ok := c.operations[request.Kind]
	if !ok {
		return model.JobStatus{}, fmt.Errorf("unsupported maintenance job %q", request.Kind)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return model.JobStatus{}, ErrControllerClosed
	}
	for _, existing := range c.jobs {
		if existing.status.State == model.JobQueued || existing.status.State == model.JobRunning {
			c.mu.Unlock()
			return model.JobStatus{}, ErrJobRunning
		}
	}
	id, err := model.NewCorrelationID()
	if err != nil {
		c.mu.Unlock()
		return model.JobStatus{}, err
	}
	created := c.now().UTC()
	status := model.JobStatus{JobID: "job-" + id[:12], Kind: request.Kind, State: model.JobQueued, CreatedAt: created, Cancelable: true}
	jobContext, cancel := context.WithCancel(context.Background())
	current := &job{status: status, cancel: cancel, done: make(chan struct{})}
	c.jobs[status.JobID] = current
	c.pruneLocked(256)
	c.mu.Unlock()
	go c.run(jobContext, current, operation, maps.Clone(request.Options))
	return status, nil
}

func (c *Controller) Await(ctx context.Context, jobID string) (model.JobStatus, error) {
	c.mu.RLock()
	current := c.jobs[jobID]
	if current == nil {
		c.mu.RUnlock()
		return model.JobStatus{}, ErrJobNotFound
	}
	done := current.done
	c.mu.RUnlock()
	select {
	case <-done:
		return c.Status(context.Background(), jobID)
	case <-ctx.Done():
		return model.JobStatus{}, ctx.Err()
	}
}

func (c *Controller) CancelAndWait(ctx context.Context, jobID string) (model.JobStatus, error) {
	if err := c.Cancel(ctx, jobID); err != nil {
		return model.JobStatus{}, err
	}
	return c.Await(ctx, jobID)
}

// Shutdown cancels the active job, if any, and waits for its lifecycle hooks
// and operation to reach a cancellation boundary.
func (c *Controller) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	var done <-chan struct{}
	for _, current := range c.jobs {
		if current.status.State == model.JobQueued || current.status.State == model.JobRunning {
			current.cancel()
			done = current.done
			break
		}
	}
	c.mu.Unlock()
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

// Close rejects future jobs, cancels the active job, waits for its transaction
// boundary and attach hook, then discards terminal job records.
func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	var done <-chan struct{}
	for _, current := range c.jobs {
		if current.status.State == model.JobQueued || current.status.State == model.JobRunning {
			current.cancel()
			done = current.done
			break
		}
	}
	c.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.Prune(0)
	return nil
}

func (c *Controller) Prune(keep int) int {
	if keep < 0 {
		keep = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pruneLocked(keep)
}

func (c *Controller) pruneLocked(keep int) int {
	type candidate struct {
		id      string
		created time.Time
	}
	var terminal []candidate
	for id, current := range c.jobs {
		if current.status.State != model.JobQueued && current.status.State != model.JobRunning {
			terminal = append(terminal, candidate{id: id, created: current.status.CreatedAt})
		}
	}
	slices.SortFunc(terminal, func(left, right candidate) int { return left.created.Compare(right.created) })
	remove := len(terminal) - keep
	if remove < 0 {
		remove = 0
	}
	for index := 0; index < remove; index++ {
		delete(c.jobs, terminal[index].id)
	}
	return remove
}

func (c *Controller) Status(_ context.Context, jobID string) (model.JobStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	current := c.jobs[jobID]
	if current == nil {
		return model.JobStatus{}, ErrJobNotFound
	}
	return cloneStatus(current.status), nil
}

func (c *Controller) Cancel(_ context.Context, jobID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.jobs[jobID]
	if current == nil {
		return ErrJobNotFound
	}
	if !current.status.Cancelable || current.status.State != model.JobQueued && current.status.State != model.JobRunning {
		return fmt.Errorf("maintenance job cannot be canceled")
	}
	current.cancel()
	return nil
}

func (c *Controller) run(ctx context.Context, current *job, operation Operation, options map[string]any) {
	c.update(current, func(status *model.JobStatus) {
		now := c.now().UTC()
		status.State = model.JobRunning
		status.StartedAt = &now
	})
	progress := func(percent int, checkpoint string) {
		if percent < 0 {
			percent = 0
		}
		if percent > 99 {
			percent = 99
		}
		c.update(current, func(status *model.JobStatus) {
			status.Progress = percent
			status.Checkpoint = checkpoint
		})
	}
	var result map[string]any
	err := call(ctx, c.hooks.Detach)
	if err == nil {
		err = call(ctx, c.hooks.Drain)
	}
	if err == nil {
		result, err = invokeOperation(ctx, operation, options, progress)
	}
	attachErr := call(context.Background(), c.hooks.Attach)
	if err == nil {
		err = attachErr
	}
	c.update(current, func(status *model.JobStatus) {
		now := c.now().UTC()
		status.FinishedAt = &now
		status.Cancelable = false
		switch {
		case errors.Is(err, context.Canceled):
			status.State = model.JobCanceled
		case err != nil:
			status.State = model.JobFailed
			if errors.Is(err, ErrBackupInvalid) {
				status.Error = &model.ErrorBody{Code: model.ErrorAnalyticsBackupInvalid, Message: "The analytics backup is invalid."}
			} else {
				status.Error = &model.ErrorBody{Code: model.ErrorAnalyticsInternal, Message: "The analytics maintenance job failed."}
			}
		default:
			status.State = model.JobSucceeded
			status.Progress = 100
			status.Result = maps.Clone(result)
		}
	})
	close(current.done)
}

func (c *Controller) update(current *job, update func(*model.JobStatus)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	update(&current.status)
}

func call(ctx context.Context, hook func(context.Context) error) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("maintenance lifecycle panic")
		}
	}()
	return hook(ctx)
}

func invokeOperation(ctx context.Context, operation Operation, options map[string]any, progress ProgressFunc) (result map[string]any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("maintenance operation panic")
		}
	}()
	return operation(ctx, options, progress)
}

func cloneStatus(status model.JobStatus) model.JobStatus {
	status.Result = maps.Clone(status.Result)
	if status.Error != nil {
		copyError := *status.Error
		copyError.Details = append([]model.ErrorDetail(nil), status.Error.Details...)
		status.Error = &copyError
	}
	return status
}
