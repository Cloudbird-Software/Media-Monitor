package core

import (
	"context"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

func TestRunCompletesAllPages(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := NewRunner(st, obs.NewCounterMap())

	task, err := r.Submit("probe", model.JSONMap{"n": 3})
	if err != nil {
		t.Fatal(err)
	}

	pages := 0
	err = r.Run(context.Background(), task.ID, func(_ context.Context, cur model.Cursor) (model.Cursor, error) {
		pages++
		return model.Cursor{Page: cur.Page + 1, HasMore: cur.Page < 2}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}

	all, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	// 1 queued + 1 running + 3 progress + 1 done = 6 appended records
	var events int
	states := map[string]bool{}
	for _, tt := range all {
		if tt.ID != task.ID {
			continue
		}
		events++
		states[tt.State] = true
	}
	if events != 6 {
		t.Fatalf("task records = %d, want 6", events)
	}
	if !states["queued"] || !states["running"] || !states["done"] {
		t.Fatalf("task states = %v, want queued+running+done present", states)
	}
	if r.Obs.Get("tasks.completed") != 1 {
		t.Fatalf("completed counter = %d", r.Obs.Get("tasks.completed"))
	}
}

func TestRunFailurePath(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := NewRunner(st, obs.NewCounterMap())
	task, err := r.Submit("probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	boom := context.DeadlineExceeded
	_ = boom
	err = r.Run(context.Background(), task.ID, func(_ context.Context, cur model.Cursor) (model.Cursor, error) {
		return model.Cursor{}, errStep
	})
	if err == nil || err != errStep {
		t.Fatalf("err = %v, want errStep", err)
	}
	if r.Obs.Get("tasks.failed") != 1 {
		t.Fatalf("failed counter = %d", r.Obs.Get("tasks.failed"))
	}
}

var errStep = &errStr{"step failed"}

type errStr struct{ s string }

func (e *errStr) Error() string { return e.s }
