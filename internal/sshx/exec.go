package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
)

// run executes cmd on the guest without a PTY and returns its stdout.
//
// It is for pt's own setup probes, not for anything the user typed: the whole
// output is buffered, stderr is dropped, and a non-zero exit is an error rather
// than a status. Interactive is the path for user commands, and it is a
// separate function precisely because the two want opposite things from a
// failure.
func (c *Client) run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var out bytes.Buffer
	sess.Stdout = &out
	if stdin != nil {
		// Only a reader that reaches EOF may be handed to sess.Stdin: Run waits
		// for its stdin copier, so an endless reader would hang the call. Every
		// caller here passes a bytes.Reader.
		sess.Stdin = stdin
	}

	// Cancellation is expressed by closing the channel, the same way Interactive
	// does it — crypto/ssh has no context-aware Run.
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()

	if err := sess.Run(cmd); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out.Bytes(), fmt.Errorf("guest command: %w", ctxErr)
		}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return out.Bytes(), fmt.Errorf("guest command exited %d", exitErr.ExitStatus())
		}
		return out.Bytes(), fmt.Errorf("guest command: %w", err)
	}
	return out.Bytes(), nil
}
