package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/11ways/nib/internal/nibash"
	"github.com/11ways/nib/internal/server"
)

// ancestorDepth bounds how far up its process tree nib shape looks for the session it runs in.
const ancestorDepth = 32

// ancestry is the processes nib shape runs under, nearest first; tests replace it.
var ancestry = procAncestors

// procAncestors reads the process tree from /proc: the parent, its parent and so on, up to but not including init; none where /proc is
// missing.
func procAncestors() []int {
	var pids []int
	for pid := os.Getppid(); pid > 1 && len(pids) < ancestorDepth; {
		pids = append(pids, pid)
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		// The command name in parentheses may hold spaces and parentheses of its own, so the fields are read after its last ')'.
		at := bytes.LastIndexByte(stat, ')')
		if at < 0 {
			break
		}
		fields := strings.Fields(string(stat[at+1:]))
		if len(fields) < 2 {
			break
		}
		next, err := strconv.Atoi(fields[1])
		if err != nil || next == pid {
			break
		}
		pid = next
	}
	return pids
}

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// The nib server of the session nib shape runs in shapes the log with its own state; without one, or when it cannot be asked, nib
	// shape shapes it here the same way.
	if answer, ok := server.AskShape(ctx, ancestry(), server.ShapeRequest{Log: path, Purpose: r.purpose, Command: r.command, Exit: r.exit}); ok {
		if answer.Status != 0 {
			fmt.Fprintln(os.Stderr, "nib shape:", answer.Error)
			return answer.Status
		}
		fmt.Println(answer.Text)
		return 0
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
	fmt.Println(nibash.ShapeLog(ctx, raw, path, r.command, r.purpose, r.exit, choice.New(), choice.Summarizer()))
	return 0
}
