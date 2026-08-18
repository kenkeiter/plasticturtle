package shell

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostileReader fails the test if anything reads from it.
//
// It stands in for a terminal that must never be prompted: the shell that loses
// the race to create an instance has nothing to ask the user about, and asking
// anyway is the bug this file exists for.
type hostileReader struct {
	t   *testing.T
	mu  sync.Mutex
	hit bool
}

func (r *hostileReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.hit = true
	r.mu.Unlock()
	return 0, nil
}

func (r *hostileReader) prompted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hit
}

// Two simultaneous first-run shells must produce exactly one instance, and the
// loser must not prompt.
//
// Before the create path claimed the project before negotiating ports, both
// shells probed the host ports: the first bound them, the second saw its own
// sibling's probe as EADDRINUSE, and asked the user to resolve a conflict that
// did not exist — then discarded the answer and attached to the winner anyway.
//
// Spec section 11 asks for exactly this test ("N goroutines racing pt shell").
func TestConcurrentFirstRunsCreateOneInstance(t *testing.T) {
	const cfgWithPort = `version: 1
image: test-image
ports:
  - vm_port: 3000
`
	h := newBareHarness(t, cfgWithPort)
	h.allow(h.hash)

	const shells = 2

	// Release every shell that reaches the decide/claim boundary together,
	// rather than waiting for a fixed count: with the correct ordering only one
	// shell gets that far, so waiting for four would hang.
	var once sync.Once
	release := make(chan struct{})

	var mu sync.Mutex
	negotiations := 0

	hookAfterDecideCreate = func() {
		once.Do(func() {
			time.AfterFunc(50*time.Millisecond, func() { close(release) })
		})
		<-release
	}
	hookBeforeNegotiate = func() {
		mu.Lock()
		negotiations++
		mu.Unlock()
	}
	t.Cleanup(func() { hookAfterDecideCreate, hookBeforeNegotiate = nil, nil })

	prompts := make([]*hostileReader, shells)
	var wg sync.WaitGroup
	errs := make([]error, shells)

	for i := 0; i < shells; i++ {
		prompts[i] = &hostileReader{t: t}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := h.opts()
			o.In = prompts[i]
			_, errs[i] = Run(context.Background(), o, h.deps())
		}(i)
	}
	wg.Wait()

	// The discriminator. With the claim before the negotiation, exactly one
	// shell ever probes the host ports; with it after, every shell released by
	// the barrier probes them and all but one sees its sibling's probe as a
	// conflict.
	mu.Lock()
	got := negotiations
	mu.Unlock()
	if got > 1 {
		t.Errorf("%d shells negotiated host ports concurrently; only the one that claimed the project may", got)
	}

	// Exactly one shell may have spawned a supervisor. The rest either attached
	// or failed cleanly; none may have created a second instance.
	if n := len(h.spawn.recorded()); n > 1 {
		t.Errorf("%d supervisors spawned for one project; the claim did not serialize", n)
	}

	for i, r := range prompts {
		if r.prompted() {
			t.Errorf("shell %d prompted about a port conflict that only its own sibling's probe created", i)
		}
	}

	// No shell may have reported a port conflict either, however it resolved.
	if got := h.out.String() + h.err.String(); strings.Contains(got, "is in use on the host") {
		t.Errorf("a shell reported a host port conflict against its own sibling:\n%s", got)
	}

	for i, err := range errs {
		if err != nil && !strings.Contains(err.Error(), "boot") && !strings.Contains(err.Error(), "context") {
			t.Logf("shell %d: %v", i, err)
		}
	}
}
