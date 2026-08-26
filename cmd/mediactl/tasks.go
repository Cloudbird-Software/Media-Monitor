package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/Cloudbird-Software/Media-Monitor/internal/core"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// cmdTasks dispatches the tasks subcommands.
func cmdTasks(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: tasks submit --kind <k> [--config <json>] | tasks list [--data <dir>]")
	}
	switch args[0] {
	case "submit":
		return tasksSubmit(args[1:])
	case "list":
		return tasksList(args[1:])
	default:
		return fmt.Errorf("unknown tasks subcommand %q", args[0])
	}
}

// openTaskStore resolves the store directory: the --data override beats
// MEDIAMON_DATA_DIR, which beats "./data".
func openTaskStore(override string) (*store.Store, error) {
	dir := override
	if dir == "" {
		dir = os.Getenv("MEDIAMON_DATA_DIR")
	}
	if dir == "" {
		dir = "data"
	}
	return store.Open(dir)
}

// tasksSubmit submits one queued task and prints it as a JSON row.
func tasksSubmit(args []string) error {
	fs := flag.NewFlagSet("tasks submit", flag.ExitOnError)
	kind := fs.String("kind", "", "task kind: search|comments|replies|users|group_members|live_monitor|trace|flow")
	config := fs.String("config", "", "raw JSON task config object")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *kind == "" {
		return fmt.Errorf("--kind is required")
	}
	cfg := model.JSONMap{}
	if *config != "" {
		if err := json.Unmarshal([]byte(*config), &cfg); err != nil {
			return fmt.Errorf("--config must be a JSON object: %v", err)
		}
	}
	st, err := openTaskStore("")
	if err != nil {
		return err
	}
	defer st.Close()
	task, err := core.NewRunner(st, obs.NewCounterMap()).Submit(*kind, cfg)
	if err != nil {
		return err
	}
	b, err := json.Marshal(task)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// tasksList prints every persisted task as one JSON row, newest first.
func tasksList(args []string) error {
	fs := flag.NewFlagSet("tasks list", flag.ExitOnError)
	data := fs.String("data", "", "store directory (default: $MEDIAMON_DATA_DIR or ./data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	st, err := openTaskStore(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	tasks, err := core.NewRunner(st, obs.NewCounterMap()).List()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	}
	return nil
}
