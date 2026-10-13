package contracts

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Probe runs one case of a suite against a recording stand-in for *testing.T and reports whether it FAILED and
// what it said. It is how a suite is shown to have teeth: run the case against a deliberately broken
// implementation and require that it is rejected, by name, without failing the test that asked.
//
// A case written against testing.TB behaves the same under Probe as under a real test: Fatal stops the case,
// Error records and continues, a panic is a failure. A case that does not finish within the timeout is a failure
// too — a broken implementation that hangs the suite is rejected, not waited for.
func Probe(timeout time.Duration, run func(t testing.TB)) (failed bool, said []string) {
	p := &probeTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				p.fail(fmt.Sprintf("panic: %v", r))
			}
			p.runCleanups()
		}()
		run(p)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		p.fail(fmt.Sprintf("did not finish within %s", timeout))
	}
	return p.Failed(), p.said()
}

// probeTB implements the part of testing.TB a suite uses. The embedded interface is nil: a method the probe does
// not implement panics, which Probe reports as a failure rather than silently passing.
type probeTB struct {
	testing.TB
	mu       sync.Mutex
	failed   bool
	messages []string
	cleanups []func()
}

func (p *probeTB) fail(msg string) {
	p.mu.Lock()
	p.failed = true
	p.messages = append(p.messages, msg)
	p.mu.Unlock()
}

func (p *probeTB) runCleanups() {
	p.mu.Lock()
	fns := p.cleanups
	p.cleanups = nil
	p.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

func (p *probeTB) said() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.messages...)
}

func (p *probeTB) Helper()      {}
func (p *probeTB) Name() string { return "probe" }
func (p *probeTB) Failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}
func (p *probeTB) Cleanup(f func()) {
	p.mu.Lock()
	p.cleanups = append(p.cleanups, f)
	p.mu.Unlock()
}
func (p *probeTB) Log(args ...any)                 {}
func (p *probeTB) Logf(format string, args ...any) {}
func (p *probeTB) Error(args ...any)               { p.fail(strings.TrimSpace(fmt.Sprintln(args...))) }
func (p *probeTB) Errorf(format string, args ...any) {
	p.fail(fmt.Sprintf(format, args...))
}
func (p *probeTB) Fail() { p.fail("Fail called") }
func (p *probeTB) FailNow() {
	p.fail("FailNow called")
	runtime.Goexit()
}
func (p *probeTB) Fatal(args ...any) {
	p.fail(strings.TrimSpace(fmt.Sprintln(args...)))
	runtime.Goexit()
}
func (p *probeTB) Fatalf(format string, args ...any) {
	p.fail(fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (p *probeTB) Skip(args ...any)                 { runtime.Goexit() }
func (p *probeTB) Skipf(format string, args ...any) { runtime.Goexit() }
func (p *probeTB) SkipNow()                         { runtime.Goexit() }
func (p *probeTB) Skipped() bool                    { return false }
