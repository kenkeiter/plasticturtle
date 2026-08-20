package progname

import (
	"os"
	"testing"
)

func TestGet(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"installed name", []string{"/opt/homebrew/bin/plasticturtle", "shell"}, "plasticturtle"},
		{"symlink name", []string{"/opt/homebrew/bin/turtle", "shell"}, "turtle"},
		{"bare name", []string{"turtle"}, "turtle"},
		{"relative path", []string{"./bin/plasticturtle"}, "plasticturtle"},
		{"user's own alias", []string{"/usr/local/bin/pt"}, "pt"},

		// The cases where argv[0] describes something that is not a command the
		// user could type. Echoing any of these back as advice would be worse
		// than saying nothing, so they fall back to the canonical name.
		{"test binary", []string{"/tmp/go-build/shell.test"}, Default},
		{"empty argv", nil, Default},
		{"empty argv[0]", []string{""}, Default},
		{"root", []string{"/"}, Default},
	}

	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.argv
			if got := Get(); got != tc.want {
				t.Errorf("Get() with argv %q = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// The whole point of the package is that a message can be copied verbatim by
// the user, which fails if the name arrives with a directory attached.
func TestGetIsAlwaysABareName(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	os.Args = []string{"/a/very/deep/path/to/turtle"}
	if got := Get(); got != "turtle" {
		t.Errorf("Get() = %q, want a bare command name", got)
	}
}
