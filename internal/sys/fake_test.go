package sys

import (
	"context"
	"errors"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)

func TestFakeClockAfterFiresOnAdvance(t *testing.T) {
	c := NewFakeClock(epoch)
	ch := c.After(5 * time.Second)

	select {
	case <-ch:
		t.Fatal("timer fired before the clock advanced")
	default:
	}

	c.Advance(4 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired early")
	default:
	}

	c.Advance(time.Second)
	select {
	case got := <-ch:
		if want := epoch.Add(5 * time.Second); !got.Equal(want) {
			t.Errorf("fired at %v, want %v", got, want)
		}
	default:
		t.Fatal("timer did not fire at its deadline")
	}
}

func TestFakeClockAdvancePastDeadlineSetsNow(t *testing.T) {
	c := NewFakeClock(epoch)
	c.After(time.Second)
	c.Advance(10 * time.Second)
	if got, want := c.Now(), epoch.Add(10*time.Second); !got.Equal(want) {
		t.Errorf("Now() = %v, want %v", got, want)
	}
}

func TestFakeClockTickerFiresOncePerPeriod(t *testing.T) {
	c := NewFakeClock(epoch)
	tk := c.NewTicker(2 * time.Second)
	defer tk.Stop()

	// Channel depth is 1, as with time.Ticker: advancing past three periods
	// without draining must not deadlock, and must not queue three beats.
	c.Advance(6 * time.Second)
	if len(tk.C()) != 1 {
		t.Fatalf("buffered beats = %d, want 1", len(tk.C()))
	}
	<-tk.C()

	c.Advance(2 * time.Second)
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker stopped firing after being drained")
	}
}

func TestFakeClockStoppedTickerDoesNotFire(t *testing.T) {
	c := NewFakeClock(epoch)
	tk := c.NewTicker(time.Second)
	tk.Stop()
	c.Advance(10 * time.Second)
	if len(tk.C()) != 0 {
		t.Error("stopped ticker fired")
	}
}

func TestFakeClockSleepBlocksUntilAdvance(t *testing.T) {
	c := NewFakeClock(epoch)
	done := make(chan struct{})
	go func() {
		c.Sleep(3 * time.Second)
		close(done)
	}()

	c.BlockUntil(1) // without this, Advance races the goroutine's registration
	select {
	case <-done:
		t.Fatal("Sleep returned before the clock advanced")
	default:
	}

	c.Advance(3 * time.Second)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Sleep did not return after the clock advanced")
	}
}

func TestFakeRunnerReplaysScriptAndRecordsCalls(t *testing.T) {
	r := NewFakeRunner()
	r.Script("tart ip pt-1", []byte("192.168.64.5\n"), nil)

	out, err := r.Run(context.Background(), "tart", "ip", "pt-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(out), "192.168.64.5\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := r.Argvs(); len(got) != 1 || got[0] != "tart ip pt-1" {
		t.Errorf("recorded calls = %v", got)
	}
}

func TestFakeRunnerUnscriptedCommandErrors(t *testing.T) {
	r := NewFakeRunner()
	if _, err := r.Run(context.Background(), "tart", "list"); !errors.Is(err, ErrNoScript) {
		t.Fatalf("err = %v, want ErrNoScript", err)
	}
}

func TestFakeProcessWaitReturnsExitError(t *testing.T) {
	p := NewFakeProcess(4242)
	want := errors.New("boom")

	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()

	p.Exit(want)
	select {
	case got := <-waited:
		if !errors.Is(got, want) {
			t.Errorf("Wait() = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Exit")
	}

	p.Exit(errors.New("second exit")) // teardown paths race; must not panic
}
