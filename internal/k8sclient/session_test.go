package k8sclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureKubeconfig = "apiVersion: v1\nkind: Config\n# ...\n"

func TestExtractKubeconfigHappyPath(t *testing.T) {
	// PTY output uses CRLF; the markers bracket the payload with a shell
	// prompt/newline noise on either side, same as real output would have.
	raw := "some banner\r\n" + marker + "\r\n" + fixtureKubeconfig + marker + "\r\nprompt# "
	got, err := extractKubeconfig([]byte(raw), marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(fixtureKubeconfig) {
		t.Errorf("got %q, want %q", got, strings.TrimSpace(fixtureKubeconfig))
	}
}

// The regression this exists for: a live Koko session echoed
// remoteReadCommand's own text back (ssh.ECHO:0 on RequestPty is not
// honoured), so the first marker pair in the stream is the echoed command
// itself -- e.g. "echo __marker__; cat ~/.kube/config; echo __marker__" --
// not the real cat output. extractKubeconfig must use the LAST pair.
func TestExtractKubeconfigIgnoresEchoedCommand(t *testing.T) {
	echoed := "echo " + marker + "; cat ~/.kube/config; echo " + marker + "\r\n"
	raw := echoed + marker + "\r\n" + fixtureKubeconfig + marker + "\r\nprompt# "
	got, err := extractKubeconfig([]byte(raw), marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(fixtureKubeconfig) {
		t.Errorf("got %q, want %q (echoed command leaked into the result)", got, strings.TrimSpace(fixtureKubeconfig))
	}
}

func TestExtractKubeconfigMissingFirstMarker(t *testing.T) {
	if _, err := extractKubeconfig([]byte("no markers here"), marker); err == nil {
		t.Error("expected an error when no marker is present")
	}
}

func TestExtractKubeconfigMissingSecondMarker(t *testing.T) {
	raw := marker + "\r\n" + fixtureKubeconfig
	if _, err := extractKubeconfig([]byte(raw), marker); err == nil {
		t.Error("expected an error when only one marker occurrence is present")
	}
}

func TestExtractKubeconfigEmptyPayload(t *testing.T) {
	raw := marker + "\r\n" + marker
	if _, err := extractKubeconfig([]byte(raw), marker); err == nil {
		t.Error("expected an error for an empty kubeconfig between markers")
	}
}

// trickleReader dribbles fixed-size chunks out with a small delay, standing
// in for a real PTY that delivers output as it's produced rather than all
// at once.
type trickleReader struct {
	remaining []byte
	chunk     int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, io.EOF
	}
	n := min(r.chunk, len(r.remaining), len(p))
	copy(p, r.remaining[:n])
	r.remaining = r.remaining[n:]
	time.Sleep(time.Millisecond)
	return n, nil
}

// recordingWriter captures what readKubeconfig sends, so tests can assert on
// WHEN the read command is sent (never before the banner).
type recordingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// spinnerText stands in for Koko's real "Connecting to Kubernetes <addr>
// <elapsed>" redraw loop (pkg/proxy/server.go's sendConnectingMsg) that
// precedes the interactive shell -- confirmed against a live Koko to take
// long enough, and produce enough spinner text containing no markers, that
// sending the read command immediately (rather than waiting for the banner)
// reliably fails.
const spinnerText = "Connecting to Kubernetes https://cluster.example.com  0.0\b\b\b0.1\b\b\b0.2"

func TestReadKubeconfigWaitsForBannerBeforeSendingCommand(t *testing.T) {
	echoedCmd := strings.TrimSuffix(remoteReadCommand, "\n")
	afterBanner := "\r\n" + bannerText + "\r\n" + echoedCmd + "\r\n" +
		marker + "\r\n" + fixtureKubeconfig + marker + "\r\nprompt# "
	r := &trickleReader{remaining: []byte(spinnerText + afterBanner), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(fixtureKubeconfig) {
		t.Errorf("got %q, want %q", got, strings.TrimSpace(fixtureKubeconfig))
	}
	if w.String() != remoteReadCommand {
		t.Errorf("sent %q, want exactly remoteReadCommand %q", w.String(), remoteReadCommand)
	}
}

// The regression this guards: sending remoteReadCommand before the banner
// races Koko's own connecting phase and (confirmed live) can pick up
// fragments of the spinner text instead of the real kubeconfig.
func TestReadKubeconfigDoesNotSendBeforeBanner(t *testing.T) {
	// Spinner text only, no banner -- readKubeconfig must still be waiting,
	// so nothing should have been written yet by the time this much has
	// streamed through.
	r := &trickleReader{remaining: []byte(spinnerText), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = readKubeconfig(ctx, w, r, 50*time.Millisecond, time.Minute)
	if w.String() != "" {
		t.Errorf("sent %q before the banner ever appeared", w.String())
	}
}

// The regression this exists for: a live Koko session returned "koko
// returned an empty kubeconfig" instead of the real, useful error --
// because the k8s connection itself had failed server-side
// (conn_k8s.go's IsValidK8sUserToken rejecting the token), and Koko's own
// error text was never looked for.
func TestReadKubeconfigSurfacesConnectError(t *testing.T) {
	raw := "Connecting to Kubernetes https://cluster.example.com error: invalid token\r\n"
	r := &trickleReader{remaining: []byte(raw), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("err = %v, want it to surface koko's own \"invalid token\" message", err)
	}
	if w.String() != "" {
		t.Errorf("sent %q after a connect error -- there is no shell to send it to", w.String())
	}
}

func TestReadKubeconfigTimesOutIfBannerNeverArrives(t *testing.T) {
	r := &trickleReader{remaining: []byte(spinnerText), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := readKubeconfig(ctx, w, r, 100*time.Millisecond, time.Minute)
	if err == nil {
		t.Fatal("expected an error when the banner never arrives")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v -- should have given up around the 100ms bannerWait", elapsed)
	}
}

// TestReadKubeconfigTimesOutIfShellNeverAnswers is the counterpart to the
// banner timeout above, and it guards a hang that shipped: bannerWait is a
// one-shot time.After, so once the command had been sent its select arm could
// never fire again and phase 2 ran with no deadline at all. A shell that
// printed its banner and then went quiet -- bash wedged, ~/.kube/config
// unreadable, `exec su` hung -- blocked readKubeconfig forever, leaving the
// connect modal stuck on "waiting for koko's kubectl session…" with Esc the
// only way out.
//
// The reader here stays open and silent after the banner, which is exactly
// that situation: no EOF to fall back on, no further bytes.
func TestReadKubeconfigTimesOutIfShellNeverAnswers(t *testing.T) {
	r := &silentAfterReader{prefix: []byte(bannerText + "\r\n")}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := readKubeconfig(ctx, w, r, 2*time.Second, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the shell never returns the kubeconfig")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gave up only because ctx expired, so readWait is not enforced: %v", err)
	}
	if !strings.Contains(err.Error(), "never returned the kubeconfig") {
		t.Errorf("err = %v, want it to name the phase that timed out", err)
	}
	// The command must have gone out -- otherwise this test would be passing
	// for the banner-timeout reason instead of the one it exists for.
	if w.String() != remoteReadCommand {
		t.Errorf("sent %q, want remoteReadCommand -- phase 2 was never entered", w.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v -- should have given up around the 150ms readWait", elapsed)
	}
}

// silentAfterReader delivers prefix, then blocks forever without returning
// EOF -- an SSH channel whose remote has stopped writing but not hung up.
// io.EOF would let readKubeconfig exit through its own EOF branch, which is a
// different path from the one under test.
type silentAfterReader struct {
	prefix []byte
	done   bool
}

func (r *silentAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.prefix), nil
	}
	// Parks indefinitely. readKubeconfig's reader goroutine is left here when
	// the deadline fires; its quit channel is what releases it.
	<-make(chan struct{})
	return 0, io.EOF
}

// The command is echoed back twice before it ever runs -- once by the tty
// driver (it is written while koko's init script still owns the terminal)
// and once by bash's readline. Neither echo may contribute a marker, or the
// read stops early and slices out the echoed command text or a bare newline
// instead of the kubeconfig (the intermittent empty-kubeconfig failure).
func TestReadKubeconfigIgnoresBothEchoesOfTheCommand(t *testing.T) {
	echoed := strings.TrimSuffix(remoteReadCommand, "\n")
	afterBanner := "\r\n" + bannerText + "\r\n" +
		echoed + "\r\n" + // tty driver echo
		"cluster# " + echoed + "\r\n" + // bash readline echo
		marker + fixtureKubeconfig + marker + "\r\ncluster# "
	r := &trickleReader{remaining: []byte(spinnerText + afterBanner), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(fixtureKubeconfig) {
		t.Errorf("got %q, want the kubeconfig %q", got, strings.TrimSpace(fixtureKubeconfig))
	}
}

// markerLiteral is what makes the test above possible: the command carries
// the marker split across two shell strings, so the marker only ever appears
// in output when the shell actually runs the command.
func TestRemoteReadCommandDoesNotContainTheMarker(t *testing.T) {
	if strings.Contains(remoteReadCommand, marker) {
		t.Fatalf("remoteReadCommand %q contains the marker verbatim -- echoing it would produce phantom markers", remoteReadCommand)
	}
	if strings.Count(remoteReadCommand, markerLiteral) != 2 {
		t.Fatalf("remoteReadCommand %q should emit the marker exactly twice", remoteReadCommand)
	}
}

// Both markers present means the whole payload has arrived; there is nothing
// further to wait for.
func TestReadKubeconfigReturnsAsSoonAsBothMarkersArrive(t *testing.T) {
	afterBanner := "\r\n" + bannerText + "\r\n" +
		marker + fixtureKubeconfig + marker + "\r\ncluster# " + strings.Repeat("x", 10_000)
	r := &trickleReader{remaining: []byte(spinnerText + afterBanner), chunk: 8}
	w := &recordingWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	got, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.TrimSpace(fixtureKubeconfig) {
		t.Errorf("got %q, want %q", got, strings.TrimSpace(fixtureKubeconfig))
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("took %v -- should have returned on the second marker, not waited", elapsed)
	}
}

func TestReadKubeconfigReturnsErrorOnPrematureEOF(t *testing.T) {
	afterBanner := "\r\n" + bannerText + "\r\n" + marker + "only one marker"
	r := &trickleReader{remaining: []byte(spinnerText + afterBanner), chunk: 16}
	w := &recordingWriter{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second); err == nil {
		t.Error("expected an error when the stream ends before even one marker pair appears")
	}
}

// blockingReader never returns, simulating a hung/dead SSH session so
// context cancellation is the only way out.
type blockingReader struct{ block chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.block
	return 0, io.EOF
}

func TestReadKubeconfigRespectsContextCancellation(t *testing.T) {
	r := &blockingReader{block: make(chan struct{})}
	defer close(r.block)
	w := &recordingWriter{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readKubeconfig(ctx, w, r, 2*time.Second, 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestKeepAliveReportsFailedPing(t *testing.T) {
	done := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	boom := errors.New("connection reset")
	go keepAlive(func() error { return boom }, time.Millisecond, stop, done)

	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("want the ping's own error wrapped, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a failed ping must be reported on done -- otherwise nothing stops k9s")
	}
}

func TestKeepAliveReportsHungPing(t *testing.T) {
	done := make(chan error, 1)
	stop := make(chan struct{})
	release := make(chan struct{})
	defer close(stop)
	defer close(release)

	// A connection dropped without an RST: the ping neither succeeds nor
	// fails, it just never returns.
	go keepAlive(func() error { <-release; return nil }, 10*time.Millisecond, stop, done)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped answering") {
			t.Fatalf("want a stopped-answering error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a ping that never returns must still be reported on done")
	}
}

func TestKeepAliveStaysQuietWhileHealthy(t *testing.T) {
	done := make(chan error, 1)
	stop := make(chan struct{})

	go keepAlive(func() error { return nil }, time.Millisecond, stop, done)
	time.Sleep(50 * time.Millisecond)
	close(stop)

	select {
	case err := <-done:
		t.Fatalf("a healthy connection must report nothing, got %v", err)
	default:
	}
}

func TestKeepAliveDoesNotBlockWhenDoneAlreadyFilled(t *testing.T) {
	// Open's io.Copy goroutine may deliver session.Wait()'s result first;
	// keepAlive must not wedge trying to add a second value.
	done := make(chan error, 1)
	done <- errors.New("session already exited")
	stop := make(chan struct{})
	defer close(stop)

	finished := make(chan struct{})
	go func() {
		keepAlive(func() error { return errors.New("boom") }, time.Millisecond, stop, done)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("keepAlive blocked on a full done channel")
	}
}

// The reader goroutine must not outlive readKubeconfig. Open starts its own
// drain on the same stdout the moment this returns, and an unbuffered send
// with nobody left listening would park the first one forever -- holding a
// goroutine and its buffer for the life of the process, and leaving two
// readers on one SSH channel until it did.
func TestReadKubeconfigLeavesNoReaderBehind(t *testing.T) {
	before := runtime.NumGoroutine()

	// Trailing content matters: the reader has to come back from one more
	// Read before it can notice it is no longer wanted.
	afterBanner := "\r\n" + bannerText + "\r\n" +
		marker + fixtureKubeconfig + marker + "\r\ncluster# " + strings.Repeat("x", 4096)
	r := &trickleReader{remaining: []byte(spinnerText + afterBanner), chunk: 8}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := readKubeconfig(ctx, &recordingWriter{}, r, 2*time.Second, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	for range 200 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stdout reader still running a second after readKubeconfig returned (%d goroutines, started at %d)",
		runtime.NumGoroutine(), before)
}
