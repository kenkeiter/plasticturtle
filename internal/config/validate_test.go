package config

import (
	"strings"
	"testing"
)

// valid returns a Config that passes Validate, for mutation by table cases.
func valid() *Config {
	return &Config{Version: SchemaVersion, Image: "ghcr.io/cirruslabs/macos-tahoe-base:latest"}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string // "" = must pass; otherwise a substring of the error
	}{
		// version
		{name: "version ok", cfg: valid()},
		{name: "version missing", cfg: &Config{Image: "img"}, want: "version: must be 1"},
		{name: "version wrong", cfg: &Config{Version: 2, Image: "img"}, want: "version: must be 1"},

		// image
		{name: "image missing", cfg: &Config{Version: 1}, want: "image: required"},
		{name: "image empty", cfg: &Config{Version: 1, Image: ""}, want: "image: required"},
		{name: "image blank", cfg: &Config{Version: 1, Image: "   "}, want: "image: required"},

		// resources
		{name: "cpu at floor", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{CPU: 1}}},
		{name: "cpu high", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{CPU: 16}}},
		{name: "cpu zero inherits", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{CPU: 0, Memory: 1024}}},
		{name: "cpu negative", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{CPU: -1}}, want: "resources.cpu: must be >= 1"},
		{name: "memory at floor", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{Memory: 512}}},
		{name: "memory below floor", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{Memory: 511}}, want: "resources.memory: must be >= 512"},
		{name: "memory negative", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{Memory: -8}}, want: "resources.memory: must be >= 512"},
		{name: "empty resources block inherits", cfg: &Config{Version: 1, Image: "img", Resources: &Resources{}}},

		// ports
		{name: "ports ok", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000}, {VMPort: 5432, HostPort: 15432}}}},
		{name: "port bounds ok", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 1}, {VMPort: 65535}}}},
		{name: "vm_port zero", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 0}}}, want: "ports[0].vm_port: must be in 1..65535"},
		{name: "vm_port negative", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: -1}}}, want: "ports[0].vm_port"},
		{name: "vm_port too high", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 65536}}}, want: "ports[0].vm_port"},
		{name: "host_port too high", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000, HostPort: 70000}}}, want: "ports[0].host_port: must be in 1..65535"},
		{name: "host_port negative", cfg: &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000, HostPort: -3}}}, want: "ports[0].host_port"},
		{
			name: "duplicate explicit host ports",
			cfg:  &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000, HostPort: 8080}, {VMPort: 4000, HostPort: 8080}}},
			want: "ports[1]: host port 8080 is already mapped by ports[0]",
		},
		{
			// The subtle one: the collision only exists after host_port defaults
			// to vm_port, so a purely textual duplicate check would miss it.
			name: "duplicate after defaulting host_port to vm_port",
			cfg:  &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000}, {VMPort: 9000, HostPort: 3000}}},
			want: "ports[1]: host port 3000 is already mapped by ports[0]",
		},
		{
			name: "same vm_port on distinct host ports is fine",
			cfg:  &Config{Version: 1, Image: "img", Ports: []Port{{VMPort: 3000, HostPort: 3000}, {VMPort: 3000, HostPort: 3001}}},
		},

		// mounts
		{name: "mounts ok", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "data-set_1", HostPath: "/tmp"}}}},
		{name: "mount name with slash", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "a/b", HostPath: "/tmp"}}}, want: `mounts[0].name: "a/b" must match [a-zA-Z0-9_-]+`},
		{name: "mount name with space", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "my data", HostPath: "/tmp"}}}, want: "must match"},
		{name: "mount name with dot", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "a.b", HostPath: "/tmp"}}}, want: "must match"},
		{name: "mount name with metacharacter", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "a;rm -rf", HostPath: "/tmp"}}}, want: "must match"},
		{name: "mount name empty", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{HostPath: "/tmp"}}}, want: "mounts[0].name: required"},
		{
			name: "duplicate mount names",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "data", HostPath: "/tmp"}, {Name: "data", HostPath: "/var"}}},
			want: `mounts[1].name: "data" is already used by mounts[0]`,
		},
		{name: "non-project mount without host_path", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "data"}}}, want: `mounts[0].host_path: required for mount "data"`},
		{name: "non-project mount with blank host_path", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "data", HostPath: "  "}}}, want: "host_path: required"},
		{name: "project mount without host_path", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: ProjectMountName, Mode: ModeRO}}}},
		{
			name: "project mount with host_path is forbidden",
			cfg:  &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: ProjectMountName, HostPath: "/elsewhere"}}},
			want: `host_path is not allowed on the reserved "project" mount`,
		},
		{name: "mode rw", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "d", HostPath: "/tmp", Mode: ModeRW}}}},
		{name: "mode ro", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "d", HostPath: "/tmp", Mode: ModeRO}}}},
		{name: "mode unknown", cfg: &Config{Version: 1, Image: "img", Mounts: []Mount{{Name: "d", HostPath: "/tmp", Mode: "rwx"}}}, want: `mounts[0].mode: must be "rw" or "ro"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsEveryViolation(t *testing.T) {
	// A user fixing a config should see all of it at once, not one error per
	// pt allow round trip.
	cfg := &Config{
		Version:   7,
		Image:     "",
		Resources: &Resources{CPU: -2, Memory: 8},
		Ports:     []Port{{VMPort: 0}, {VMPort: 3000}, {VMPort: 9000, HostPort: 3000}},
		Mounts: []Mount{
			{Name: "bad name", HostPath: "/tmp"},
			{Name: "data"},
			{Name: ProjectMountName, HostPath: "/elsewhere"},
			{Name: "data", HostPath: "/tmp"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil")
	}
	want := []string{
		"version: must be 1",
		"image: required",
		"resources.cpu",
		"resources.memory",
		"ports[0].vm_port",
		"host port 3000 is already mapped",
		"mounts[0].name",
		`mounts[1].host_path: required for mount "data"`,
		"host_path is not allowed on the reserved",
		`mounts[3].name: "data" is already used`,
	}
	got := err.Error()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(got), "\n")); n != len(want) {
		t.Errorf("got %d violations, want %d:\n%s", n, len(want), got)
	}
}

func TestValidateNilConfig(t *testing.T) {
	var c *Config
	if err := c.Validate(); err == nil {
		t.Error("Validate on nil = nil, want error")
	}
}
