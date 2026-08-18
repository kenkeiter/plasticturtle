package tart

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

const baseImage = "ghcr.io/cirruslabs/macos-tahoe-base:latest"

func TestFakeCloneThenList(t *testing.T) {
	ctx := context.Background()
	f := NewFake(baseImage)

	if err := f.Clone(ctx, baseImage, "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	vms, err := f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []VM{
		{Source: SourceOCI, Name: baseImage, State: StateStopped},
		{Source: SourceLocal, Name: "pt-1", State: StateStopped},
	}
	if !reflect.DeepEqual(vms, want) {
		t.Fatalf("List:\n got %+v\nwant %+v", vms, want)
	}
	if got := f.Existing(); !reflect.DeepEqual(got, []string{baseImage, "pt-1"}) {
		t.Fatalf("Existing = %q", got)
	}
}

func TestFakeCloneRejectsUnknownImageAndDuplicates(t *testing.T) {
	ctx := context.Background()
	f := NewFake(baseImage)

	if err := f.Clone(ctx, "no-such-image", "pt-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := f.Clone(ctx, baseImage, "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := f.Clone(ctx, baseImage, "pt-1"); err == nil {
		t.Fatal("cloning over an existing VM succeeded")
	}
}

func TestFakeDeleteRemovesTheClone(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := f.Delete(ctx, "pt-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := f.Existing(); !reflect.DeepEqual(got, []string{"tahoe-base"}) {
		t.Fatalf("Existing = %q, want only the seed image", got)
	}
	if err := f.Delete(ctx, "pt-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := f.Set(ctx, "pt-1", 4, 8192); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if _, err := f.IP(ctx, "pt-1"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("IP on a stopped VM = %v, want ErrNoIP", err)
	}

	p, err := f.Run(ctx, "pt-1", RunOpts{NoGraphics: true, Dirs: []DirShare{{Name: "project", HostPath: "/src"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.Process("pt-1") != p {
		t.Fatal("Process did not return the handle Run handed out")
	}
	if _, err := f.Run(ctx, "pt-1", RunOpts{}); err == nil {
		t.Fatal("running an already-running VM succeeded")
	}

	if _, err := f.IP(ctx, "pt-1"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("IP before SetIP = %v, want ErrNoIP", err)
	}
	f.SetIP("pt-1", "192.168.64.9")
	ip, err := f.IP(ctx, "pt-1")
	if err != nil || ip != "192.168.64.9" {
		t.Fatalf("IP = %q, %v", ip, err)
	}

	vms, err := f.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if vms[0].Name != "pt-1" || vms[0].State != StateRunning {
		t.Fatalf("List = %+v, want pt-1 running", vms)
	}

	// Stopping must release the run handle, the way a real `tart stop` makes
	// the `tart run` child exit.
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	if err := f.Stop(ctx, "pt-1", false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Wait after Stop: %v", err)
	}
	// Teardown stops twice (graceful, then force); neither may look like an error.
	if err := f.Stop(ctx, "pt-1", true); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, err := f.IP(ctx, "pt-1"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("IP after Stop = %v, want ErrNoIP", err)
	}
}

func TestFakeDeleteReleasesARunningVM(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	p, err := f.Run(ctx, "pt-1", RunOpts{NoGraphics: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := f.Delete(ctx, "pt-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait after Delete: %v", err)
	}
	// The handle outlives the VM so teardown assertions can still inspect it.
	if f.Process("pt-1") == nil {
		t.Fatal("Process forgot the handle after Delete")
	}
}

func TestFakeUnexpectedDeath(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := f.Run(ctx, "pt-1", RunOpts{NoGraphics: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	boom := errors.New("exit status 1")
	f.Process("pt-1").Exit(boom)
	if err := f.Process("pt-1").Wait(); !errors.Is(err, boom) {
		t.Fatalf("Wait = %v, want %v", err, boom)
	}
}

func TestFakeMissingVM(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	tests := map[string]func() error{
		"Set":    func() error { return f.Set(ctx, "pt-1", 4, 0) },
		"Run":    func() error { _, err := f.Run(ctx, "pt-1", RunOpts{}); return err },
		"Stop":   func() error { return f.Stop(ctx, "pt-1", false) },
		"Delete": func() error { return f.Delete(ctx, "pt-1") },
		"IP":     func() error { _, err := f.IP(ctx, "pt-1"); return err },
	}
	for name, call := range tests {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	// Mirrors the CLI: an all-zero Set never reaches tart, so it cannot fail.
	if err := f.Set(ctx, "pt-1", 0, 0); err != nil {
		t.Errorf("zero-valued Set = %v, want nil", err)
	}
}

func TestFakeFailNextInjectsExactlyOneFailure(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")
	boom := errors.New("tart is grumpy")

	f.FailNext("Clone", boom)
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); !errors.Is(err, boom) {
		t.Fatalf("first Clone = %v, want the injected error", err)
	}
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); err != nil {
		t.Fatalf("second Clone = %v, want success", err)
	}
	// The failed call must not have had side effects.
	if got := f.Existing(); !reflect.DeepEqual(got, []string{"pt-1", "tahoe-base"}) {
		t.Fatalf("Existing = %q", got)
	}

	f.FailNext("ip", boom) // method names are matched case-insensitively
	if _, err := f.IP(ctx, "pt-1"); !errors.Is(err, boom) {
		t.Fatalf("IP = %v, want the injected error", err)
	}
	if _, err := f.IP(ctx, "pt-1"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("IP = %v, want ErrNoIP once the injection is spent", err)
	}
}

func TestFakeHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewFake("tahoe-base")
	if err := f.Clone(ctx, "tahoe-base", "pt-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Clone = %v, want context.Canceled", err)
	}
	if _, err := f.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List = %v, want context.Canceled", err)
	}
}

// TestFakeConcurrent is the point of the mutex: the supervisor polls IP and
// watches sessions from separate goroutines while teardown runs.
func TestFakeConcurrent(t *testing.T) {
	ctx := context.Background()
	f := NewFake("tahoe-base")

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "pt-" + string(rune('a'+i))
			_ = f.Clone(ctx, "tahoe-base", name)
			_ = f.Set(ctx, name, 2, 4096)
			if p, err := f.Run(ctx, name, RunOpts{NoGraphics: true}); err == nil && p == nil {
				t.Error("Run returned a nil handle without an error")
			}
			f.SetIP(name, "192.168.64.10")
			_, _ = f.IP(ctx, name)
			_, _ = f.List(ctx)
			_ = f.Existing()
			_ = f.Process(name)
			_ = f.Stop(ctx, name, true)
			_ = f.Delete(ctx, name)
		}(i)
	}
	// Readers racing the writers above.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = f.List(ctx)
				_ = f.Existing()
			}
		}()
	}
	wg.Wait()

	if got := f.Existing(); !reflect.DeepEqual(got, []string{"tahoe-base"}) {
		t.Fatalf("Existing = %q, want every clone deleted", got)
	}
}
