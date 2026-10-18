package decider

import "sync/atomic"

// Observer hears each model call as it starts, of the kind "decider" or "summary" and the model or entry asked, and gets back the function
// to call when the call ends. It lets a client show what runs while a long output waits.
type Observer func(kind, model string) (end func())

// Observable is a decider or summarizer that tells an Observer of the model calls it makes; call Observe before serving.
type Observable interface{ Observe(Observer) }

// observers holds the Observer a decider or summarizer tells of its calls.
type observers struct{ observer atomic.Pointer[Observer] }

// Observe sets the Observer.
func (o *observers) Observe(f Observer) { o.observer.Store(&f) }

// begin tells the Observer a call starts and returns the function that ends it; with none set, it does nothing.
func (o *observers) begin(kind, model string) func() {
	if f := o.observer.Load(); f != nil && *f != nil {
		if end := (*f)(kind, model); end != nil {
			return end
		}
	}
	return func() {}
}
