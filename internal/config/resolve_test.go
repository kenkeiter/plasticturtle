package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectWithDirs makes a canonical project dir plus the named subdirectories.
func projectWithDirs(t *testing.T, subdirs ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range subdirs {
		if err := os.MkdirAll(filepath.Join(root, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveDefaults(t *testing.T) {
	root := projectWithDirs(t)
	cfg := &Config{Version: 1, Image: "  img  ", Ports: []Port{{VMPort: 3000}, {VMPort: 5432, HostPort: 15432}}}

	res, err := cfg.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ProjectPath != root {
		t.Errorf("ProjectPath = %q, want %q", res.ProjectPath, root)
	}
	if res.Image != "img" {
		t.Errorf("Image = %q, want trimmed", res.Image)
	}
	if res.CPU != 0 || res.Memory != 0 {
		t.Errorf("CPU/Memory = %d/%d, want inherit (0/0)", res.CPU, res.Memory)
	}
	want := []ResolvedPort{{VMPort: 3000, HostPort: 3000}, {VMPort: 5432, HostPort: 15432}}
	if len(res.Ports) != len(want) {
		t.Fatalf("Ports = %+v", res.Ports)
	}
	for i, w := range want {
		if res.Ports[i] != w {
			t.Errorf("Ports[%d] = %+v, want %+v", i, res.Ports[i], w)
		}
	}
	// The project mount exists even though the file never mentioned it.
	if len(res.Mounts) != 1 || res.Mounts[0] != (ResolvedMount{Name: ProjectMountName, HostPath: root, Mode: ModeRW}) {
		t.Errorf("Mounts = %+v, want the implicit rw project mount", res.Mounts)
	}
}

func TestResolveResources(t *testing.T) {
	root := projectWithDirs(t)
	cfg := &Config{Version: 1, Image: "img", Resources: &Resources{CPU: 8, Memory: 8192}}
	res, err := cfg.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.CPU != 8 || res.Memory != 8192 {
		t.Errorf("CPU/Memory = %d/%d", res.CPU, res.Memory)
	}
}

func TestResolveProjectMountMode(t *testing.T) {
	root := projectWithDirs(t)
	for _, tc := range []struct {
		name  string
		mount []Mount
		want  Mode
	}{
		{name: "absent defaults to rw", want: ModeRW},
		{name: "explicit ro", mount: []Mount{{Name: ProjectMountName, Mode: ModeRO}}, want: ModeRO},
		{name: "explicit rw", mount: []Mount{{Name: ProjectMountName, Mode: ModeRW}}, want: ModeRW},
		{name: "entry without mode stays rw", mount: []Mount{{Name: ProjectMountName}}, want: ModeRW},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Version: 1, Image: "img", Mounts: tc.mount}
			res, err := cfg.Resolve(root)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(res.Mounts) != 1 {
				t.Fatalf("Mounts = %+v, want only the project mount", res.Mounts)
			}
			got := res.Mounts[0]
			if got.Name != ProjectMountName || got.HostPath != root || got.Mode != tc.want {
				t.Errorf("project mount = %+v, want mode %q at %q", got, tc.want, root)
			}
		})
	}
}

func TestResolvePathExpansion(t *testing.T) {
	root := projectWithDirs(t, "scratch", "nested/deep")
	home := projectWithDirs(t, "datasets")
	t.Setenv("HOME", home)

	cfg := &Config{
		Version: 1,
		Image:   "img",
		Mounts: []Mount{
			{Name: "tilde", HostPath: "~/datasets", Mode: ModeRO},
			{Name: "tildehome", HostPath: "~"},
			{Name: "dotrel", HostPath: "./scratch"},
			{Name: "bareRel", HostPath: "nested/deep"},
			{Name: "updown", HostPath: "./nested/../scratch"},
			{Name: "abs", HostPath: root},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	res, err := cfg.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []ResolvedMount{
		{Name: ProjectMountName, HostPath: root, Mode: ModeRW}, // always first
		{Name: "tilde", HostPath: filepath.Join(home, "datasets"), Mode: ModeRO},
		{Name: "tildehome", HostPath: home, Mode: ModeRW},
		{Name: "dotrel", HostPath: filepath.Join(root, "scratch"), Mode: ModeRW},
		{Name: "bareRel", HostPath: filepath.Join(root, "nested", "deep"), Mode: ModeRW},
		{Name: "updown", HostPath: filepath.Join(root, "scratch"), Mode: ModeRW},
		{Name: "abs", HostPath: root, Mode: ModeRW},
	}
	if len(res.Mounts) != len(want) {
		t.Fatalf("Mounts = %+v, want %d entries", res.Mounts, len(want))
	}
	for i, w := range want {
		if res.Mounts[i] != w {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, res.Mounts[i], w)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	root := projectWithDirs(t, "exists")
	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		cfg     *Config
		dir     string
		want    string
		wantAll []string
	}{
		{
			name: "missing mount source",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "gone", HostPath: "./nope"}}},
			dir:  root,
			want: "does not exist",
		},
		{
			name: "missing home-relative mount source",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "gone", HostPath: "~/definitely-not-here"}}},
			dir:  root,
			want: "does not exist",
		},
		{
			name: "mount source is a file",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "f", HostPath: file}}},
			dir:  root,
			want: "is not a directory",
		},
		{
			name: "missing project dir",
			cfg:  &Config{Version: 1, Image: "img"},
			dir:  filepath.Join(root, "vanished"),
			want: `mount "project"`,
		},
		{
			name: "relative project dir",
			cfg:  &Config{Version: 1, Image: "img"},
			dir:  "relative/path",
			want: "is not absolute",
		},
		{
			// Validate rejects this too, but Resolve must not turn an empty
			// host_path into a share of the whole project directory.
			name: "empty host_path",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "empty", HostPath: ""}}},
			dir:  root,
			want: "host_path is empty",
		},
		{
			name: "tilde user paths unsupported",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "other", HostPath: "~bob/data"}}},
			dir:  root,
			want: "~user paths are not supported",
		},
		{
			name: "every missing source is reported at once",
			cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{
				{Name: "a", HostPath: "./nope-a"},
				{Name: "b", HostPath: "./nope-b"},
				{Name: "ok", HostPath: "./exists"},
			}},
			dir:     root,
			wantAll: []string{`mount "a"`, `mount "b"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.cfg.Resolve(tc.dir)
			if err == nil {
				t.Fatalf("Resolve = %+v, want error", res)
			}
			if res != nil {
				t.Errorf("Resolve returned %+v alongside an error; callers must get nothing usable", res)
			}
			for _, w := range append(tc.wantAll, tc.want) {
				if w == "" {
					continue
				}
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), `mount "ok"`) {
				t.Errorf("err = %v, mentions a mount that exists", err)
			}
		})
	}
}

func TestResolveNilConfig(t *testing.T) {
	var c *Config
	if _, err := c.Resolve("/tmp"); err == nil {
		t.Error("Resolve on nil = nil, want error")
	}
}

func TestLoadValidateResolveRoundTrip(t *testing.T) {
	// End to end over the real file: the path every command takes.
	root := projectWithDirs(t, "scratch")
	body := `version: 1
image: ghcr.io/cirruslabs/macos-tahoe-base:latest
resources:
  cpu: 4
  memory: 4096
ports:
  - vm_port: 3000
  - vm_port: 5432
    host_port: 15432
mounts:
  - name: project
    mode: ro
  - name: scratch
    host_path: ./scratch
`
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := Find(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	cfg, raw, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(raw) != body {
		t.Error("raw bytes differ from the file")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	res, err := cfg.Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Mounts[0].Mode != ModeRO {
		t.Errorf("project mount mode = %q, want ro", res.Mounts[0].Mode)
	}
	if res.Mounts[1].HostPath != filepath.Join(root, "scratch") {
		t.Errorf("scratch host path = %q", res.Mounts[1].HostPath)
	}
	if res.Ports[0].HostPort != 3000 || res.Ports[1].HostPort != 15432 {
		t.Errorf("Ports = %+v", res.Ports)
	}
}
