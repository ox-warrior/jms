package sshclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
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
	return session.Wait()
}
