//go:build darwin || freebsd || netbsd

package platform

import "syscall"

func changeTime(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }
