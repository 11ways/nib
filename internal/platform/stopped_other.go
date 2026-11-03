//go:build unix && !linux && !darwin

package platform

// Stopped does not know whether a program is stopped: this OS gives nib no way to ask that leaves the stop's report to the program's waiter.
func Stopped(int) (stopped, known bool) { return false, false }
