package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/11ways/nib/internal/nibash"
)

// shapeRequest is what nib shape is asked: the log, the run's description, its exit status and the command that wrote it.
type shapeRequest struct {
	log, purpose, command string
	exit                  int
}

// shapeFlags reads nib shape's flags, each as --name VALUE or --name=VALUE; --log and --purpose are required.
func shapeFlags(args []string) (shapeRequest, error) {
	var r shapeRequest
	given := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch name {
		case "--log", "--purpose", "--exit", "--command":
		default:
			return r, fmt.Errorf("unknown option %s", args[i])
		}
		if given[name] {
			return r, fmt.Errorf("%s is given twice", name)
		}
		given[name] = true
		if !inline {
			if i+1 == len(args) {
				return r, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "--log":
			r.log = value
		case "--purpose":
			r.purpose = value
		case "--command":
			r.command = value
		case "--exit":
			n, err := strconv.Atoi(value)
			if err != nil {
				return r, fmt.Errorf("--exit takes a number, not %q", value)
			}
			r.exit = n
		}
	}
	if !given["--log"] || r.log == "" {
		return r, fmt.Errorf("--log PATH is required")
	}
	if !given["--purpose"] {
		return r, fmt.Errorf("--purpose TEXT is required")
	}
	return r, nil
}

// runShape prints what nibash would show of a program whose whole output is a log, with the deciders this environment chooses.
func runShape(args []string) int {
	r, err := shapeFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib shape: %v\n", err)
		return 2
	}
	path, err := filepath.Abs(r.log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib shape: %v\n", err)
		return 2
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib shape: %v\n", err)
		return 2
	}
	if r.command == "" {
		r.command = "cat " + path
	}
	choice, err := deciderChoice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "nib shape:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	fmt.Println(nibash.ShapeLog(ctx, raw, path, r.command, r.purpose, r.exit, choice.New(), choice.Summarizer()))
	return 0
}
