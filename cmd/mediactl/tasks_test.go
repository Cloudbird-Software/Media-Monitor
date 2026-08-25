package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns the
// captured output.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), runErr
}

func TestTasksSubmitAndList(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("MEDIAMON_DATA_DIR", data)

	// Empty store: list prints nothing.
	out, err := captureStdout(t, func() error { return cmdTasks([]string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("empty list printed %q", out)
	}

	// Submit a task.
	out, err = captureStdout(t, func() error {
		return cmdTasks([]string{"submit", "--kind", "search", "--config", `{"kw":"x","limit":3}`})
	})
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]any
	if err := json.Unmarshal([]byte(out), &task); err != nil {
		t.Fatalf("submit output %q is not JSON: %v", out, err)
	}
	if task["kind"] != "search" || task["state"] != "queued" {
		t.Fatalf("task = %v", task)
	}
	if task["config"].(map[string]any)["kw"] != "x" {
		t.Fatalf("config = %v", task["config"])
	}
	id := task["id"]

	// List returns it newest first with the same id.
	out, err = captureStdout(t, func() error { return cmdTasks([]string{"list", "--data", data}) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("list output = %q", out)
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &listed); err != nil {
		t.Fatal(err)
	}
	if listed["id"] != id || listed["created_at"] == nil {
		t.Fatalf("listed task = %v", listed)
	}
}

func TestTasksSubmitValidation(t *testing.T) {
	t.Setenv("MEDIAMON_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	if err := tasksSubmit([]string{}); err == nil || !strings.Contains(err.Error(), "--kind is required") {
		t.Fatalf("empty submit error = %v", err)
	}
	if err := tasksSubmit([]string{"--kind", "search", "--config", "{not json"}); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("bad config error = %v", err)
	}
	if err := cmdTasks([]string{"nope"}); err == nil {
		t.Fatal("unknown tasks subcommand accepted")
	}
}
