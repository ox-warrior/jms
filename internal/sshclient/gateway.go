package sshclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

const tokenUsernamePrefix = "JMS-"

// TokenGatewayOptions authenticates to Koko with a JumpServer connection
// token instead of an SSH keypair: the token ID rides in the SSH username
// (prefixed with "JMS-") and the token value rides in the SSH password.
// Koko validates the token against Core and, on success, never asks for MFA
// (docs/PLAN.md §5.A.1-2). Used for the per-asset connect flow in
// internal/tui/app.go's connectResource, once jmsd has minted a ticket.
type TokenGatewayOptions struct {
	Address    string
	TokenID    string
	TokenValue string
	HostKey    ssh.HostKeyCallback
	Timeout    time.Duration
}

func ConnectGatewayWithToken(ctx context.Context, opts TokenGatewayOptions) (*ssh.Client, error) {
	if opts.TokenID == "" || opts.TokenValue == "" {
		return nil, errors.New("token id and value are required")
	}
	if opts.HostKey == nil {
		return nil, errors.New("host key verification is required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	config := &ssh.ClientConfig{
		User:            tokenUsernamePrefix + opts.TokenID,
		Auth:            []ssh.AuthMethod{ssh.Password(opts.TokenValue)},
		HostKeyCallback: opts.HostKey,
		Timeout:         opts.Timeout,
	}
	return dialAndHandshake(ctx, opts.Address, config, opts.Timeout)
}

func dialAndHandshake(ctx context.Context, address string, config *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect Koko %s: %w", address, err)
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-stop:
		}
	}()
	conn, channels, requests, err := ssh.NewClientConn(raw, address, config)
	close(stop)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("authenticate Koko: %w", err)
	}
	return ssh.NewClient(conn, channels, requests), nil
}

func RunInteractiveShell(client *ssh.Client) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("interactive shell requires a terminal")
	}
	width, height, err := term.GetSize(fd)
	if err != nil {
		return err
	}
	terminalName := os.Getenv("TERM")
	if terminalName == "" {
		terminalName = "xterm-256color"
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty(terminalName, height, width, modes); err != nil {
		return err
	}
	// Koko reports an asset it cannot reach by printing why and then exiting
	// cleanly, so Wait() on its own cannot tell that apart from a logout --
	// which is how an unreachable asset used to end up reported as "session
	// ended". These two wrappers are what makes the difference visible:
	// keystrokes get counted, and the opening bytes of output get kept.
	stdin := &countReader{r: os.Stdin}
	head := &headTap{}
	session.Stdin = stdin
	session.Stdout = io.MultiWriter(os.Stdout, head)
	// Left untapped on purpose: a PTY was requested just above, so the remote
	// merges its stderr into the pty and it arrives on the stdout channel
	// anyway. Tapping both would also mean two of x/crypto/ssh's copy
	// goroutines writing one headTap with no lock between them.
	session.Stderr = os.Stderr

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, oldState)

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	// Deferred in this order so they run in the other one: signal.Stop first,
	// which guarantees no further sends, and only then the close that lets the
	// watcher below return. Without the close it blocks on the channel for the
	// life of the process -- one leaked goroutine per session, each one pinning
	// the ssh.Session it captured, in a client whose whole point is opening
	// session after session from one long-running TUI.
	defer close(resize)
	defer signal.Stop(resize)
	go func() {
		for range resize {
			w, h, sizeErr := term.GetSize(fd)
			if sizeErr == nil {
				_ = session.WindowChange(h, w)
			}
		}
	}()

	if err := session.Shell(); err != nil {
		return err
	}
	// Leaving a shell takes a keystroke -- `exit`, or Ctrl-D. Zero of them
	// means the remote hung up by itself, and the text it printed on the way
	// out is the only explanation anyone gets.
	//
	// head.text() is read after Wait() returns on purpose: Wait() drains
	// x/crypto/ssh's stdout copy goroutine, which is what establishes that
	// everything Koko sent has reached the tap.
	//
	// ponytail: head only, so a session that dies after the user has worked
	// in it (Koko's idle timeout) shows the banner rather than the reason.
	// Keeping the tail would put the user's own commands in the log file;
	// give the tail its own small ring buffer if that case ever matters.
	waitErr := session.Wait()
	return sessionOutcome(waitErr, stdin.n.Load(), head.text())
}

// sessionOutcome decides what a finished session is reported as. Split out of
// RunInteractiveShell only so it can be tested without a live Koko.
//
// Koko exits 0 on an asset it could not reach, which is why waitErr cannot be
// the whole answer -- but it does not always, so the captured text has to win
// over a non-nil waitErr rather than be skipped by it.
func sessionOutcome(waitErr error, typedBytes int64, output string) error {
	if typedBytes > 0 {
		return waitErr
	}
	const hungUp = "remote closed the session without any input from us"
	switch {
	case output != "":
		return fmt.Errorf("%s:\n%s", hungUp, output)
	case waitErr != nil:
		return fmt.Errorf("%s: %w", hungUp, waitErr)
	default:
		return errors.New(hungUp)
	}
}

// headTapMax caps the surviving text, not the bytes written: output that gets
// overwritten costs nothing, so a redraw can spin indefinitely without
// crowding out the error that follows it.
const headTapMax = 2048

// headTap keeps the first headTapMax bytes of output a terminal would still
// be showing, and discards the rest.
//
// The head rather than the tail is a security boundary, not a memory one:
// what this captures is Koko's line about why it could not reach the asset,
// printed before the user could have typed anything, so what gets kept can
// never include a command the user ran or its output. That is what makes it
// safe to put in a log file.
type headTap struct {
	lines []string // finished lines
	cur   []byte   // the line being written
	kept  int      // len(lines)+len(cur), measured against headTapMax
	done  bool     // cap reached; every later write is a no-op
	// pendingCR holds the decision about a \r until the next byte: with \n it
	// is a CRLF line ending, alone it is an overwrite. On the struct rather
	// than in Write's loop so a \r and \n split across two writes still read
	// as one ending.
	pendingCR bool
}

// Write acts on the bytes that decide what a terminal would still be showing
// and treats everything else as text:
//
//	\b    un-prints the character just written. This is the one Koko uses for
//	      its elapsed-seconds counter: "0.0", then \b\b\b and the next frame.
//	\r    alone, sends the cursor back to column 0, so the line was painted
//	      over. Not what Koko does today, but the other standard way to do it.
//	\n    ends the line; it can no longer be overwritten.
//	\r\n  a line ending -- the \r here overwrites nothing.
//
// Never reports a short write or an error: a headTap sits in an io.MultiWriter
// ahead of the real stdout, and MultiWriter treats either as fatal for every
// writer in the list -- including the one driving the user's terminal.
func (t *headTap) Write(p []byte) (int, error) {
	if t.done {
		return len(p), nil
	}
	for _, b := range p {
		if t.pendingCR {
			t.pendingCR = false
			if b == '\n' {
				t.endLine()
				continue
			}
			t.kept -= len(t.cur)
			t.cur = t.cur[:0]
		}
		switch b {
		case '\r':
			t.pendingCR = true
		case '\n':
			t.endLine()
		case '\b':
			t.dropRune()
		default:
			if t.kept < headTapMax {
				t.cur = append(t.cur, b)
				t.kept++
				continue
			}
			t.endLine()
			t.done = true
			return len(p), nil
		}
	}
	return len(p), nil
}

// dropRune un-prints the last rune of the current line. A whole rune, so
// backing over a multi-byte character cannot leave half of one behind.
func (t *headTap) dropRune() {
	if len(t.cur) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(t.cur)
	t.cur = t.cur[:len(t.cur)-size]
	t.kept -= size
}

// endLine moves the current line into lines. kept already counts every byte
// of t.cur, so only what the trim discards is given back.
func (t *headTap) endLine() {
	line := strings.TrimRight(string(t.cur), " \t")
	t.kept -= len(t.cur) - len(line)
	if line != "" {
		t.lines = append(t.lines, line)
	}
	t.cur = t.cur[:0]
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// text renders the captured lines for a log file and a tview modal, neither
// of which interprets escape sequences.
func (t *headTap) text() string {
	all := t.lines
	// The unterminated last line counts: a Koko that dies mid-line still
	// wrote the only explanation there is. Unless a \r is pending, which with
	// no byte behind it can only have been an overwrite -- the session is
	// over, so the \n that would have made it an ending is not coming.
	if len(t.cur) > 0 && !t.pendingCR {
		all = append(all[:len(all):len(all)], string(t.cur))
	}
	out := make([]string, 0, len(all))
	for _, line := range all {
		line = strings.TrimSpace(ansiEscape.ReplaceAllString(line, ""))
		line = strings.Map(func(r rune) rune {
			if unicode.IsGraphic(r) {
				return r
			}
			return -1
		}, line)
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(collapseSharedPrefix(out), "\n")
}

// minSharedPrefix is long enough that ordinary openings ("error: ",
// "开始连接到 ") do not collapse on their own.
const minSharedPrefix = 16

// collapseSharedPrefix indents the opening a line shares with the one above
// it instead of repeating it. Koko announces the same
// "开始连接到 <user>@<addr>" twice, once with its counter and once with the
// failure, which in a modal narrow enough to wrap is two copies of the same
// 45 characters.
func collapseSharedPrefix(lines []string) []string {
	out := make([]string, len(lines))
	copy(out, lines)
	for i := 1; i < len(lines); i++ {
		// Against the untouched previous line, so three or more lines sharing
		// an opening indent from it rather than from each other.
		shared := sharedPrefixLen(lines[i-1], lines[i])
		if shared < minSharedPrefix || shared == len(lines[i]) {
			continue
		}
		out[i] = "  " + strings.TrimLeft(lines[i][shared:], " ")
	}
	return out
}

// sharedPrefixLen is the length in bytes of the common prefix of a and b,
// backed up to a rune boundary -- splitting a multi-byte character would put
// invalid UTF-8 into the indented remainder.
func sharedPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	// n can land on len(b), where there is no byte to inspect.
	for n > 0 && n < len(b) && !utf8.RuneStart(b[n]) {
		n--
	}
	return n
}

// countReader counts the bytes read through it.
//
// Atomic because the reader being wrapped is os.Stdin, and x/crypto/ssh
// drains that on a goroutine it never waits for: session.stdin() copies it
// into an io.Pipe, and Wait() finishes by closing the pipe's write end rather
// than by waiting for that copy. So the count gets read while the goroutine
// feeding it is still parked in Read.
type countReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
