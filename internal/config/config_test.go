package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProject creates dir (relative to a fresh temp root) containing a
// .plasticturtle with the given body, and returns the temp root.
func writeProject(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const minimalConfig = "version: 1\nimage: ghcr.io/cirruslabs/macos-tahoe-base:latest\n"

func TestFind(t *testing.T) {
	root := writeProject(t, minimalConfig)
	canonRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("at project root", func(t *testing.T) {
		got, err := Find(root)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if got != canonRoot {
			t.Errorf("Find = %q, want %q", got, canonRoot)
		}
	})

	t.Run("from a nested subdirectory", func(t *testing.T) {
		sub := filepath.Join(root, "a", "b", "c")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := Find(sub)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if got != canonRoot {
			t.Errorf("Find = %q, want %q", got, canonRoot)
		}
	})

	t.Run("through a symlink yields the same canonical project", func(t *testing.T) {
		// A symlink pointing at a subdirectory of the project: the upward walk
		// must follow the target's ancestry, not the link's.
		sub := filepath.Join(root, "src")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(sub, link); err != nil {
			t.Fatal(err)
		}
		got, err := Find(link)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if got != canonRoot {
			t.Errorf("Find through symlink = %q, want %q", got, canonRoot)
		}
	})

	t.Run("absent anywhere above", func(t *testing.T) {
		// t.TempDir() lives under /var/folders, which has no .plasticturtle at
		// any ancestor, so this exercises the walk all the way to /.
		if _, err := Find(t.TempDir()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a directory named .plasticturtle is not a config", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Find(dir); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestLoad(t *testing.T) {
	t.Run("returns exact bytes and parsed config", func(t *testing.T) {
		body := "version: 1\nimage: local-image\n\n# trailing comment\n"
		root := writeProject(t, body)
		cfg, raw, err := Load(root)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if string(raw) != body {
			t.Errorf("raw = %q, want the exact file bytes %q", raw, body)
		}
		if cfg.Version != 1 || cfg.Image != "local-image" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("missing file is ErrNotFound", func(t *testing.T) {
		if _, _, err := Load(t.TempDir()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("full config decodes", func(t *testing.T) {
		root := writeProject(t, `version: 1
image: img
resources:
  cpu: 8
  memory: 8192
ports:
  - vm_port: 3000
    host_port: 3001
  - vm_port: 5432
mounts:
  - name: project
    mode: ro
  - name: datasets
    host_path: ~/datasets
    mode: ro
`)
		cfg, _, err := Load(root)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Resources == nil || cfg.Resources.CPU != 8 || cfg.Resources.Memory != 8192 {
			t.Errorf("resources = %+v", cfg.Resources)
		}
		if len(cfg.Ports) != 2 || cfg.Ports[1].HostPort != 0 {
			t.Errorf("ports = %+v", cfg.Ports)
		}
		if len(cfg.Mounts) != 2 || cfg.Mounts[0].Mode != ModeRO || cfg.Mounts[1].HostPath != "~/datasets" {
			t.Errorf("mounts = %+v", cfg.Mounts)
		}
	})
}

func TestLoadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	// An unreadable config is a real error, not "no project here": telling the
	// user to run pt init would be the wrong advice.
	root := writeProject(t, minimalConfig)
	if err := os.Chmod(filepath.Join(root, FileName), 0o000); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("Load succeeded on an unreadable file")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a read error rather than ErrNotFound", err)
	}
}

func TestLoadStrictDecoding(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring of the expected error
	}{
		{
			name: "unknown top-level key",
			body: "version: 1\nimage: img\nfirewall: bridged\n",
			want: "firewall",
		},
		{
			name: "unknown nested key under network",
			body: "version: 1\nimage: img\nnetwork:\n  policy: restricted\n  allowlist: [github.com]\n",
			want: "allowlist",
		},
		{
			name: "unknown nested key under resources",
			body: "version: 1\nimage: img\nresources:\n  cpu: 2\n  gpu: 1\n",
			want: "gpu",
		},
		{
			name: "unknown nested key under mounts",
			body: "version: 1\nimage: img\nmounts:\n  - name: data\n    host_path: /tmp\n    readonly: true\n",
			want: "readonly",
		},
		{
			name: "unknown nested key under ports",
			body: "version: 1\nimage: img\nports:\n  - vm_port: 3000\n    protocol: udp\n",
			want: "protocol",
		},
		{
			name: "misspelled key is not silently ignored",
			body: "version: 1\nimage: img\nmounts:\n  - name: data\n    hostpath: /tmp\n",
			want: "hostpath",
		},
		{
			name: "wrong type",
			body: "version: one\nimage: img\n",
			want: "cannot unmarshal",
		},
		{
			name: "empty file",
			body: "",
			want: "no configuration",
		},
		{
			name: "comments only",
			body: "# nothing here\n",
			want: "no configuration",
		},
		{
			name: "multiple documents",
			body: "version: 1\nimage: img\n---\nversion: 1\nimage: evil\n",
			want: "more than one YAML document",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(writeProject(t, tc.body))
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadAcceptsWellFormedConfigs(t *testing.T) {
	// The negative case for strict decoding: every documented key parses.
	bodies := []string{
		minimalConfig,
		"version: 1\nimage: img\nresources:\n  cpu: 8\n",
		"version: 1\nimage: img\nresources:\n  memory: 8192\n",
		"version: 1\nimage: img\nports:\n  - vm_port: 3000\n",
		"version: 1\nimage: img\nmounts:\n  - name: project\n    mode: ro\n",
	}
	for _, body := range bodies {
		cfg, _, err := Load(writeProject(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", body, err)
		}
	}
}

func TestHashBytes(t *testing.T) {
	// Known vector: sha256("") — proves the format and that we hash the bytes
	// given, not a re-serialization of them.
	const emptySHA = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := HashBytes(nil); got != emptySHA {
		t.Errorf("HashBytes(nil) = %q, want %q", got, emptySHA)
	}
	if got := HashBytes([]byte{}); got != emptySHA {
		t.Errorf("HashBytes(empty) = %q", got)
	}

	// Byte-exactness: a semantically identical file with different bytes must
	// hash differently, or "re-allow on change" would not hold.
	a := HashBytes([]byte("version: 1\nimage: img\n"))
	b := HashBytes([]byte("version: 1\nimage: img\n# comment\n"))
	if a == b {
		t.Error("configs differing only in a comment hashed the same")
	}
	if !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Errorf("hash %q is not sha256:<64 hex>", a)
	}
}

func TestGuestProjectPath(t *testing.T) {
	if got := GuestProjectPath(); got != "/Volumes/My Shared Files/project" {
		t.Errorf("GuestProjectPath = %q", got)
	}
}
