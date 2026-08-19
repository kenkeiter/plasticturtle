package sys

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// FakeClock is a Clock whose time advances only when a test says so. It lives
// in the ordinary build, not a _test.go file, because packages other than sys
// need it — the supervisor's lifecycle tests are the whole reason it exists.
type FakeClock struct {
	mu      sync.Mutex
	changed sync.Cond
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	deadline time.Time
	period   time.Duration // nonzero for tickers
	ch       chan time.Time
	stopped  bool
}

// NewFakeClock returns a FakeClock started at t.
func NewFakeClock(t time.Time) *FakeClock {
	c := &FakeClock{now: t}
	c.changed.L = &c.mu
	return c
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) Sleep(d time.Duration) { <-c.After(d) }

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &waiter{deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.waiters = append(c.waiters, w)
	c.changed.Broadcast()
	return w.ch
}

func (c *FakeClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("sys: FakeClock.NewTicker: non-positive interval")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &waiter{deadline: c.now.Add(d), period: d, ch: make(chan time.Time, 1)}
	c.waiters = append(c.waiters, w)
	c.changed.Broadcast()
	return &fakeTicker{c: c, w: w}
}

type fakeTicker struct {
	c *FakeClock
	w *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }

func (t *fakeTicker) Stop() {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.w.stopped = true
}

// Advance moves the clock forward by d, firing every timer and ticker that
// comes due along the way. Tickers fire once per elapsed period, in order, so
// advancing past several intervals is equivalent to advancing through them.
//
// Sends are non-blocking on a buffered channel, matching time.Ticker: a
// consumer that has not drained its channel misses beats rather than
// deadlocking the test.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	target := c.now.Add(d)
	for {
		next, idx := c.earliestDue(target)
		if idx < 0 {
			break
		}
		c.now = next
		w := c.waiters[idx]
		select {
		case w.ch <- c.now:
		default:
		}
		if w.period > 0 {
			w.deadline = w.deadline.Add(w.period)
		} else {
			c.waiters = append(c.waiters[:idx], c.waiters[idx+1:]...)
		}
	}
	c.now = target
	c.changed.Broadcast()
}

// earliestDue returns the soonest waiter due at or before limit.
func (c *FakeClock) earliestDue(limit time.Time) (time.Time, int) {
	best := -1
	var bestAt time.Time
	for i, w := range c.waiters {
		if w.stopped || w.deadline.After(limit) {
			continue
		}
		if best < 0 || w.deadline.Before(bestAt) {
			best, bestAt = i, w.deadline
		}
	}
	return bestAt, best
}

// BlockUntil waits until n goroutines are waiting on this clock.
//
// Tests need it because Advance is racy against a watcher goroutine that has
// not yet reached its first Sleep: advancing before the waiter registers fires
// nothing, and the test hangs.
func (c *FakeClock) BlockUntil(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.activeWaiters() < n {
		c.changed.Wait()
	}
}

func (c *FakeClock) activeWaiters() int {
	n := 0
	for _, w := range c.waiters {
		if !w.stopped {
			n++
		}
	}
	return n
}

// ErrNoScript is returned by FakeRunner for an unscripted command. It is an
// error rather than a zero-value success so that a test with a typo in its
// expected argv fails loudly.
var ErrNoScript = errors.New("sys: no scripted response for command")

// Call is one recorded invocation.
type Call struct {
	Name string
	Args []string
	// Env holds the extra environment entries passed to StartEnv, if any. It is
	// nil for Run and plain Start.
	Env []string
}

// String renders a call as the argv a test would script.
func (c Call) String() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

type response struct {
	stdout []byte
	err    error
}

// FakeRunner records calls and replays scripted responses keyed by full argv.
type FakeRunner struct {
	mu      sync.Mutex
	scripts map[string]response
	calls   []Call
	procs   map[string]*FakeProcess
	nextPID int
}

// NewFakeRunner returns an empty FakeRunner.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		scripts: map[string]response{},
		procs:   map[string]*FakeProcess{},
		nextPID: 90000,
	}
}

// Script registers stdout and err for the given argv, e.g.
// "tart clone base pt-abc". A later Script for the same argv replaces it.
func (r *FakeRunner) Script(argv string, stdout []byte, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts[argv] = response{stdout: stdout, err: err}
}

// Calls returns the recorded calls in order.
func (r *FakeRunner) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Argvs returns the recorded calls as argv strings, for compact assertions.
func (r *FakeRunner) Argvs() []string {
	out := []string{}
	for _, c := range r.Calls() {
		out = append(out, c.String())
	}
	return out
}

func (r *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Call{Name: name, Args: args}
	r.calls = append(r.calls, c)
	resp, ok := r.scripts[c.String()]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoScript, c.String())
	}
	return resp.stdout, resp.err
}

func (r *FakeRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	return r.StartEnv(ctx, name, nil, args...)
}

func (r *FakeRunner) StartEnv(ctx context.Context, name string, env []string, args ...string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Call{Name: name, Args: args, Env: env}
	r.calls = append(r.calls, c)
	if resp, ok := r.scripts[c.String()]; ok && resp.err != nil {
		return nil, resp.err
	}
	p := &FakeProcess{pid: r.nextPID, done: make(chan struct{})}
	r.nextPID++
	r.procs[c.String()] = p
	return p, nil
}

// Started returns the process created by Start for the given argv, so a test
// can make a long-lived child exit.
func (r *FakeRunner) Started(argv string) *FakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[argv]
}

// FakeProcess is a Process whose exit a test controls.
type FakeProcess struct {
	mu      sync.Mutex
	pid     int
	done    chan struct{}
	err     error
	exited  bool
	signals []os.Signal
}

// NewFakeProcess returns a running FakeProcess with the given pid.
func NewFakeProcess(pid int) *FakeProcess {
	return &FakeProcess{pid: pid, done: make(chan struct{})}
}

// Exit causes any pending or future Wait to return err. Exiting twice is a
// no-op: teardown paths race, and a panic here would be an artifact of the
// fake rather than a real bug.
func (p *FakeProcess) Exit(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	p.exited = true
	p.err = err
	close(p.done)
}

// Signals returns the signals delivered so far.
func (p *FakeProcess) Signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

func (p *FakeProcess) Pid() int { return p.pid }

func (p *FakeProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *FakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, sig)
	return nil
}
