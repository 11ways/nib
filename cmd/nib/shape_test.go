package main

import (
	"path/filepath"
	"testing"
)

func TestShapeFlags(t *testing.T) {
	r, err := shapeFlags([]string{"--log", "a.log", "--purpose=run it", "--exit", "3", "--command", "zenit-dev test"})
	if err != nil || r != (shapeRequest{log: "a.log", purpose: "run it", command: "zenit-dev test", exit: 3}) {
		t.Fatalf("%+v %v", r, err)
	}
	for _, args := range [][]string{
		{"--purpose", "x"},
		{"--log", "a.log"},
		{"--log", "a.log", "--purpose", "x", "--exit", "one"},
		{"--log", "a.log", "--purpose", "x", "--log", "b.log"},
		{"--log", "a.log", "--purpose"},
		{"--log", "a.log", "--purpose", "x", "--verbose"},
	} {
		if _, err := shapeFlags(args); err == nil {
			t.Errorf("%q was taken", args)
		}
	}
}

func TestShapeRefusesBadArgumentsAndUnreadableLogsWithStatus2(t *testing.T) {
	if status := runShape([]string{"--purpose", "x"}); status != 2 {
		t.Fatalf("no log: %d", status)
	}
	if status := runShape([]string{"--log", filepath.Join(t.TempDir(), "missing.log"), "--purpose", "x"}); status != 2 {
		t.Fatalf("missing log: %d", status)
	}
	if status := runShape([]string{"--log", t.TempDir(), "--purpose", "x"}); status != 2 {
		t.Fatalf("directory: %d", status)
	}
}
