package sshx

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Interactive must forward stdin to the guest even when there is no terminal.
//
// This is a regression test for a bug found by booting a real VM: the non-TTY
// branch wired stdout and stderr but left sess.Stdin unset, so the remote shell
// received immediate EOF, ran nothing, and exited 0. `plasticturtle shell < script`
// reported success having done nothing at all.
func TestInteractiveForwardsStdinWithoutATTY(t *testing.T) {
	srv, err := NewTestServer(Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// The handler echoes whatever it is given, which is only possible if stdin
	// actually arrived.
	srv.SetExecHandler(func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(stdin); err != nil {
			return 1
		}
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			return 1
		}
		return 0
	})

	c, err := Dial(context.Background(), srv.Addr(), Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A real file, not a pipe: Interactive reads os.Stdin directly in the
	// non-TTY branch, so the test has to replace it.
	const payload = "echo from the host\n"
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	stdinSaved, stdoutSaved := os.Stdin, os.Stdout
	os.Stdin = f
	outPath := filepath.Join(t.TempDir(), "stdout")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = outFile
	t.Cleanup(func() { os.Stdin, os.Stdout = stdinSaved, stdoutSaved })

	code, err := c.Interactive(context.Background(), "cat", nil)
	_ = outFile.Close()
	os.Stdin, os.Stdout = stdinSaved, stdoutSaved

	if err != nil {
		t.Fatalf("Interactive: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "from the host") {
		t.Errorf("guest did not receive stdin; captured stdout = %q", string(got))
	}
}
