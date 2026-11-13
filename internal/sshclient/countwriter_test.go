package sshclient

import (
	"bytes"
	"sync/atomic"
	"testing"
)

// stdout and stderr share one cap, and the write that crosses it is cut
// exactly at the limit rather than dropped or passed whole.
func TestCountWriterCapsBothStreamsTogether(t *testing.T) {
	var out, errOut bytes.Buffer
	var n atomic.Int64
	overs := 0
	over := func() { overs++ }
	stdout := &countWriter{w: &out, n: &n, max: 10, over: over}
	stderr := &countWriter{w: &errOut, n: &n, max: 10, over: over}

	if _, err := stdout.Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("abcdef")); err == nil {
		t.Fatal("crossing the cap did not fail the write")
	}
	if out.String() != "123456" || errOut.String() != "abcd" || overs != 1 {
		t.Fatalf("stdout=%q stderr=%q overs=%d; want 123456, abcd, 1", out.String(), errOut.String(), overs)
	}
}
