package tunnel

import (
	"bytes"
	"errors"
	"testing"
)

// rw is a client's side of the conversation: canned request bytes to read, and
// a buffer capturing what the proxy wrote back.
type rw struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func newRW(in []byte) *rw                 { return &rw{in: bytes.NewReader(in)} }
func (c *rw) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *rw) Write(p []byte) (int, error) { return c.out.Write(p) }

func greeting() []byte { return []byte{5, 1, 0} } // one method: no auth

// domainRequest builds a CONNECT for a name, since the length byte has to
// agree with the name and hand-counting it in a table is how a test ends up
// asserting a truncated hostname.
func domainRequest(host string, port int) []byte {
	req := append([]byte{5, socksConnect, 0, 3, byte(len(host))}, host...)
	return append(req, byte(port>>8), byte(port))
}

func TestSocksHandshakeAddressTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		request  []byte
		wantHost string
		wantPort int
	}{
		{
			// A name must survive as a name: it is resolved at jmsd's end,
			// where the internal DNS is, and where the answer can be checked
			// against the allow list. Resolving it here would both fail (the
			// laptop cannot see that zone) and move the check to the wrong side.
			name:     "domain",
			request:  domainRequest("app.internal", 443),
			wantHost: "app.internal",
			wantPort: 443,
		},
		{
			name:     "ipv4",
			request:  []byte{5, 1, 0, 1, 10, 20, 1, 5, 0x15, 0x38},
			wantHost: "10.20.1.5",
			wantPort: 5432,
		},
		{
			name: "ipv6",
			request: []byte{5, 1, 0, 4,
				0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
				0x00, 0x50},
			wantHost: "2001:db8::1",
			wantPort: 80,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRW(append(greeting(), tc.request...))
			target, err := socksHandshake(c)
			if err != nil {
				t.Fatalf("socksHandshake: %v", err)
			}
			if target.Host != tc.wantHost || target.Port != tc.wantPort {
				t.Fatalf("target = %s:%d, want %s:%d", target.Host, target.Port, tc.wantHost, tc.wantPort)
			}
			// The method reply must be "version 5, no authentication".
			if got := c.out.Bytes()[:2]; got[0] != 5 || got[1] != 0 {
				t.Errorf("method reply = %v, want [5 0]", got)
			}
		})
	}
}

// BIND and UDP ASSOCIATE ask the proxy to accept inbound connections for the
// client. Refusing them is deliberate, and the refusal has to reach the client
// as "command not supported" rather than a dropped connection, or every SOCKS
// client retries it.
func TestSocksHandshakeRefusesNonConnect(t *testing.T) {
	for _, cmd := range []byte{2, 3} {
		c := newRW(append(greeting(), 5, cmd, 0, 1, 10, 20, 1, 5, 0, 80))
		_, err := socksHandshake(c)
		if !errors.Is(err, errUnsupportedCommand) {
			t.Fatalf("command %d: error = %v, want errUnsupportedCommand", cmd, err)
		}
		if code := socksFailureCode(err); code != socksCmdNotSupported {
			t.Errorf("command %d: reply code = %d, want %d", cmd, code, socksCmdNotSupported)
		}
	}
}

func TestSocksHandshakeRejectsWrongVersion(t *testing.T) {
	// SOCKS4 clients reach the port too; the failure must be an error, not a
	// misparse of the next bytes as an address.
	if _, err := socksHandshake(newRW([]byte{4, 1, 0, 80})); err == nil {
		t.Fatal("socks4 greeting accepted")
	}
}

// A truncated request must be an error rather than a zero-value target, which
// would otherwise be dialled as ":0".
func TestSocksHandshakeTruncated(t *testing.T) {
	c := newRW(append(greeting(), 5, 1, 0, 1, 10, 20)) // address cut short
	if _, err := socksHandshake(c); err == nil {
		t.Fatal("truncated request accepted; it would be dialled as \":0\"")
	}
}

// A policy refusal maps to "not allowed by ruleset" so curl and browsers say
// so, instead of reporting a generic failure for the one case the user can
// actually act on.
func TestSocksFailureCodeDistinguishesRefusal(t *testing.T) {
	if got := socksFailureCode(ErrDestinationRefused); got != socksNotAllowed {
		t.Errorf("refusal code = %d, want %d", got, socksNotAllowed)
	}
	if got := socksFailureCode(errors.New("dial tcp: i/o timeout")); got != socksHostUnreachable {
		t.Errorf("generic failure code = %d, want %d", got, socksHostUnreachable)
	}
}

// A client that only speaks username/password must be told so (RFC 1928's
// 0xFF) rather than handed "use no authentication" -- answering method 0 to a
// client that never offered it leaves it waiting for a request it will not
// send, which presents as a hang rather than an error.
func TestSocksHandshakeRefusesWhenNoAuthNotOffered(t *testing.T) {
	// Offers only 0x02 (username/password).
	c := newRW([]byte{5, 1, 2})
	_, err := socksHandshake(c)
	if !errors.Is(err, errNoAcceptableAuth) {
		t.Fatalf("error = %v, want errNoAcceptableAuth", err)
	}
	if got := c.out.Bytes(); len(got) != 2 || got[0] != 5 || got[1] != 0xFF {
		t.Errorf("reply = %v, want [5 255]", got)
	}
}

// The usual case: a client offering both 0x00 and 0x02 gets no-auth selected.
func TestSocksHandshakeSelectsNoAuthWhenOffered(t *testing.T) {
	c := newRW(append([]byte{5, 2, 0, 2}, domainRequest("app.internal", 443)...))
	target, err := socksHandshake(c)
	if err != nil {
		t.Fatalf("socksHandshake: %v", err)
	}
	if target.Host != "app.internal" || target.Port != 443 {
		t.Fatalf("target = %s:%d", target.Host, target.Port)
	}
	if got := c.out.Bytes()[:2]; got[0] != 5 || got[1] != 0 {
		t.Errorf("method reply = %v, want [5 0]", got)
	}
}
