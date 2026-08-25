// Package core implements task lifecycle management on top of the JSONL
// store: submit/list/run, with cursor persistence between steps.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// Runner owns task records and drives step loops.
type Runner struct {
	Store *store.Store
	Obs   *obs.CounterMap
	Now   func() int64
}

// NewRunner builds a Runner with default clock.
func NewRunner(st *store.Store, o *obs.CounterMap) *Runner {
	return &Runner{Store: st, Obs: o, Now: func() int64 { return time.Now().Unix() }}
}

// Submit creates a queued task and persists it.
func (r *Runner) Submit(kind string, cfg model.JSONMap) (model.Task, error) {
	now := r.Now()
	t := model.Task{
		ID:        newTaskID(now),
		Kind:      kind,
		Config:    cfg,
		State:     "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
	return t, r.appendTask(t)
}

// List returns all persisted tasks, newest first.
func (r *Runner) List() ([]model.Task, error) {
	var out []model.Task
	err := r.Store.Scan("tasks", func(raw []byte) error {
		var t model.Task
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Run executes step until exhaustion, cancellation, or step error. Each
// iteration persists the task (state + cursor) for resume/debugging.
func (r *Runner) Run(ctx context.Context, id string, step func(ctx context.Context, cur model.Cursor) (model.Cursor, error)) error {
	t, err := r.task(id)
	if err != nil {
		return err
	}
	t.State = "running"
	t.UpdatedAt = r.Now()
	if err := r.appendTask(t); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			t.State = "cancelled"
			t.Error = err.Error()
			t.UpdatedAt = r.Now()
			_ = r.appendTask(t)
			if r.Obs != nil {
				r.Obs.Inc("tasks.cancelled", 1)
			}
			return err
		}
		next, err := step(ctx, t.Cursor)
		if err != nil {
			t.State = "failed"
			t.Error = err.Error()
			t.UpdatedAt = r.Now()
			_ = r.appendTask(t)
			if r.Obs != nil {
				r.Obs.Inc("tasks.failed", 1)
			}
			return err
		}
		t.Cursor = next
		t.Progress++
		t.UpdatedAt = r.Now()
		if err := r.appendTask(t); err != nil {
			return err
		}
		if !next.HasMore {
			t.State = "done"
			t.UpdatedAt = r.Now()
			if err := r.appendTask(t); err != nil {
				return err
			}
			if r.Obs != nil {
				r.Obs.Inc("tasks.completed", 1)
			}
			return nil
		}
	}
}

func (r *Runner) task(id string) (model.Task, error) {
	var found model.Task
	err := r.Store.Scan("tasks", func(raw []byte) error {
		var t model.Task
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		if t.ID == id {
			found = t
		}
		return nil
	})
	if err != nil {
		return model.Task{}, err
	}
	if found.ID == "" {
		return model.Task{}, fmt.Errorf("task %s not found", id)
	}
	return found, nil
}

func (r *Runner) appendTask(t model.Task) error {
	return r.Store.Append("tasks", t)
}

func newTaskID(now int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x", now, b)
}
