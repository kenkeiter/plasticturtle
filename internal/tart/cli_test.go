package tart

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/sys"
)

// realListJSON is verbatim output of `tart list --format json` from tart
// 2.32.1 on macOS 26, including its key ordering (which varies per element),
// escaped slashes, and the "OCI" / "local" case split.
const realListJSON = `[
  {
    "Source" : "local",
    "Disk" : 50,
    "State" : "stopped",
    "Size" : 33,
    "Accessed" : "2026-08-18T05:06:00Z",
    "Name" : "tahoe-base",
    "Running" : false
  },
  {
    "Name" : "ghcr.io\/cirruslabs\/macos-tahoe-base:latest",
    "Source" : "OCI",
    "Accessed" : "2026-08-18T04:58:11Z",
    "State" : "stopped",
    "Size" : 33,
    "Disk" : 50,
    "Running" : false
  },
  {
    "Accessed" : "2026-08-18T04:58:11Z",
    "Running" : true,
    "State" : "running",
    "Name" : "pt-0123456789abcdef-89abcdef",
    "Disk" : 50,
    "Source" : "local",
    "Size" : 34
  }
]
`

// Verbatim stderr from tart 2.32.1, as sys.RealRunner would wrap it.
const (
	realNotFoundStderr = `tart [ip pt-missing]: exit status 2: the specified VM "pt-missing" does not exist`
	realNoIPStderr     = `tart [ip tahoe-base]: exit status 1: no IP address found, is your VM running?`
)

func newCLI(t *testing.T) (Client, *sys.FakeRunner) {
	t.Helper()
	r := sys.NewFakeRunner()
	return NewCLI("tart", r), r
}

func wantArgvs(t *testing.T, r *sys.FakeRunner, want ...string) {
	t.Helper()
	if got := r.Argvs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}
}

func TestNewCLIDefaultsBinary(t *testing.T) {
	r := sys.NewFakeRunner()
	r.Script("tart list --format json", []byte("[]"), nil)
	if _, err := NewCLI("", r).List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	wantArgvs(t, r, "tart list --format json")
}

func TestArgv(t *testing.T) {
	tests := []struct {
		name   string
		call   func(Client) error
		want   []string
		stdout string // scripted for every argv in want
	}{
		{
			name: "clone",
			call: func(c Client) error { return c.Clone(context.Background(), "tahoe-base", "pt-1") },
			want: []string{"tart clone tahoe-base pt-1"},
		},
		{
			name: "clone from oci reference",
			call: func(c Client) error {
				return c.Clone(context.Background(), "ghcr.io/cirruslabs/macos-tahoe-base:latest", "pt-1")
			},
			want: []string{"tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest pt-1"},
		},
		{
			name: "set both",
			call: func(c Client) error { return c.Set(context.Background(), "pt-1", 4, 8192) },
			want: []string{"tart set pt-1 --cpu 4 --memory 8192"},
		},
		{
			name: "set cpu only omits memory",
			call: func(c Client) error { return c.Set(context.Background(), "pt-1", 4, 0) },
			want: []string{"tart set pt-1 --cpu 4"},
		},
		{
			name: "set memory only omits cpu",
			call: func(c Client) error { return c.Set(context.Background(), "pt-1", 0, 8192) },
			want: []string{"tart set pt-1 --memory 8192"},
		},
		{
			name: "set with both zero runs nothing",
			call: func(c Client) error { return c.Set(context.Background(), "pt-1", 0, 0) },
			want: []string{},
		},
		{
			name: "stop graceful",
			call: func(c Client) error { return c.Stop(context.Background(), "pt-1", false) },
			want: []string{"tart stop pt-1"},
		},
		{
			name: "stop force",
			call: func(c Client) error { return c.Stop(context.Background(), "pt-1", true) },
			want: []string{"tart stop pt-1 --timeout 0"},
		},
		{
			name: "delete",
			call: func(c Client) error { return c.Delete(context.Background(), "pt-1") },
			want: []string{"tart delete pt-1"},
		},
		{
			name: "ip",
			call: func(c Client) error { _, err := c.IP(context.Background(), "pt-1"); return err },
			want: []string{"tart ip pt-1"},
		},
		{
			name:   "list",
			call:   func(c Client) error { _, err := c.List(context.Background()); return err },
			want:   []string{"tart list --format json"},
			stdout: "[]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, r := newCLI(t)
			stdout := tc.stdout
			if stdout == "" {
				stdout = "192.168.64.5\n"
			}
			for _, argv := range tc.want {
				r.Script(argv, []byte(stdout), nil)
			}
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			wantArgvs(t, r, tc.want...)
		})
	}
}

func TestRunArgv(t *testing.T) {
	tests := []struct {
		name string
		opts RunOpts
		want string
	}{
		{
			name: "no shares",
			opts: RunOpts{NoGraphics: true},
			want: "tart run --no-graphics pt-1",
		},
		{
			name: "rw and ro shares",
			opts: RunOpts{NoGraphics: true, Dirs: []DirShare{
				{Name: "project", HostPath: "/Users/x/src/app"},
				{Name: "cache", HostPath: "/Users/x/.cache", ReadOnly: true},
			}},
			want: "tart run --no-graphics --dir=project:/Users/x/src/app --dir=cache:/Users/x/.cache:ro pt-1",
		},
		{
			name: "unnamed share omits the name prefix",
			opts: RunOpts{NoGraphics: true, Dirs: []DirShare{{HostPath: "/Users/x/src/app", ReadOnly: true}}},
			want: "tart run --no-graphics --dir=/Users/x/src/app:ro pt-1",
		},
		{
			name: "graphics left on when the caller says so",
			opts: RunOpts{},
			want: "tart run pt-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, r := newCLI(t)
			p, err := c.Run(context.Background(), "pt-1", tc.opts)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if p == nil {
				t.Fatal("Run returned a nil process")
			}
			wantArgvs(t, r, tc.want)
			// Start, not Run: the handle must be a live child the caller waits on.
			if r.Started(tc.want) == nil {
				t.Fatalf("Run did not go through Runner.Start")
			}
		})
	}
}

func TestRunStartFailurePropagates(t *testing.T) {
	c, r := newCLI(t)
	boom := errors.New("fork/exec: resource temporarily unavailable")
	r.Script("tart run --no-graphics pt-1", nil, boom)
	if _, err := c.Run(context.Background(), "pt-1", RunOpts{NoGraphics: true}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		argv   string
		stderr string
		call   func(Client) error
		want   error
	}{
		{
			name:   "clone of a missing source",
			argv:   "tart clone nope pt-1",
			stderr: `tart [clone nope pt-1]: exit status 2: the specified VM "nope" does not exist`,
			call:   func(c Client) error { return c.Clone(context.Background(), "nope", "pt-1") },
			want:   ErrNotFound,
		},
		{
			name:   "set on a missing vm",
			argv:   "tart set pt-missing --cpu 4",
			stderr: `tart [set pt-missing --cpu 4]: exit status 2: the specified VM "pt-missing" does not exist`,
			call:   func(c Client) error { return c.Set(context.Background(), "pt-missing", 4, 0) },
			want:   ErrNotFound,
		},
		{
			name:   "stop of a missing vm",
			argv:   "tart stop pt-missing",
			stderr: `tart [stop pt-missing]: exit status 2: the specified VM "pt-missing" does not exist`,
			call:   func(c Client) error { return c.Stop(context.Background(), "pt-missing", false) },
			want:   ErrNotFound,
		},
		{
			name:   "delete of a missing vm",
			argv:   "tart delete pt-missing",
			stderr: `tart [delete pt-missing]: exit status 2: the specified VM "pt-missing" does not exist`,
			call:   func(c Client) error { return c.Delete(context.Background(), "pt-missing") },
			want:   ErrNotFound,
		},
		{
			name:   "ip of a missing vm",
			argv:   "tart ip pt-missing",
			stderr: realNotFoundStderr,
			call:   func(c Client) error { _, err := c.IP(context.Background(), "pt-missing"); return err },
			want:   ErrNotFound,
		},
		{
			name:   "ip before the guest has a lease",
			argv:   "tart ip tahoe-base",
			stderr: realNoIPStderr,
			call:   func(c Client) error { _, err := c.IP(context.Background(), "tahoe-base"); return err },
			want:   ErrNoIP,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, r := newCLI(t)
			r.Script(tc.argv, nil, errors.New(tc.stderr))
			err := tc.call(c)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUnknownFailureIsNotClassified(t *testing.T) {
	c, r := newCLI(t)
	// A full disk must not be mistaken for a missing VM: best-effort teardown
	// ignores ErrNotFound, and swallowing this would hide a real problem.
	r.Script("tart clone tahoe-base pt-1", nil, errors.New("tart [clone tahoe-base pt-1]: exit status 2: not enough disk space"))
	err := c.Clone(context.Background(), "tahoe-base", "pt-1")
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoIP) {
		t.Fatalf("err = %v, want an unclassified error", err)
	}
	if err == nil {
		t.Fatal("Clone succeeded on a failing command")
	}
}

func TestMissingBinaryIsNotErrNotFound(t *testing.T) {
	c, r := newCLI(t)
	r.Script("tart ip pt-1", nil, fmt.Errorf(`exec: "tart": executable file not found in $PATH`))
	if err := func() error { _, err := c.IP(context.Background(), "pt-1"); return err }(); errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing tart binary was reported as a missing VM: %v", err)
	}
}

func TestIPTrimsAndRejectsEmpty(t *testing.T) {
	t.Run("trims", func(t *testing.T) {
		c, r := newCLI(t)
		r.Script("tart ip pt-1", []byte("192.168.64.7\n"), nil)
		ip, err := c.IP(context.Background(), "pt-1")
		if err != nil || ip != "192.168.64.7" {
			t.Fatalf("IP = %q, %v", ip, err)
		}
	})
	t.Run("empty output is ErrNoIP", func(t *testing.T) {
		c, r := newCLI(t)
		r.Script("tart ip pt-1", []byte("\n"), nil)
		if _, err := c.IP(context.Background(), "pt-1"); !errors.Is(err, ErrNoIP) {
			t.Fatalf("err = %v, want ErrNoIP", err)
		}
	})
}

func TestListParsesRealOutput(t *testing.T) {
	c, r := newCLI(t)
	r.Script("tart list --format json", []byte(realListJSON), nil)
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []VM{
		{Source: SourceLocal, Name: "tahoe-base", DiskGB: 50, SizeGB: 33, State: StateStopped},
		{Source: SourceOCI, Name: "ghcr.io/cirruslabs/macos-tahoe-base:latest", DiskGB: 50, SizeGB: 33, State: StateStopped},
		{Source: SourceLocal, Name: "pt-0123456789abcdef-89abcdef", DiskGB: 50, SizeGB: 34, State: StateRunning},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List:\n got %+v\nwant %+v", got, want)
	}
}

func TestListEmptyAndInvalid(t *testing.T) {
	t.Run("empty output", func(t *testing.T) {
		c, r := newCLI(t)
		r.Script("tart list --format json", []byte("  \n"), nil)
		got, err := c.List(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("List = %v, %v", got, err)
		}
	})
	t.Run("garbage output", func(t *testing.T) {
		c, r := newCLI(t)
		r.Script("tart list --format json", []byte("not json"), nil)
		if _, err := c.List(context.Background()); err == nil {
			t.Fatal("List accepted non-JSON output")
		}
	})
	t.Run("command failure", func(t *testing.T) {
		c, r := newCLI(t)
		r.Script("tart list --format json", nil, errors.New("exit status 2"))
		if _, err := c.List(context.Background()); err == nil {
			t.Fatal("List swallowed a command failure")
		}
	})
}
