//go:build linux || openbsd || dragonfly || solaris || aix

package platform

import "syscall"

func changeTime(st *syscall.Stat_t) int64 { return st.Ctim.Nano() }
