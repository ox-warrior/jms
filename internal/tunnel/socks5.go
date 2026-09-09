// SOCKS5, client side only: the loopback front door that turns "point your
// browser/psql/redis-cli at 127.0.0.1:9090" into a stream request jmsd can
// authorise. Only the two exchanges that matter are implemented -- the method
// greeting and a CONNECT request -- because BIND and UDP ASSOCIATE both need
// the proxy to accept inbound connections on the caller's behalf, which is
// exactly the capability a tunnel like this should not hand out.
//
// Reads are exact-length (io.ReadFull), never buffered. SOCKS5 forbids a
// client from sending payload before it has read the reply, so there is
// nothing to over-read -- and a bufio.Reader here would silently swallow the
// first bytes of the stream if any client ever broke that rule.
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

const socksVersion = 5

// SOCKS5 reply codes (RFC 1928 §6). Only the ones this proxy can actually
// produce are named.
const (
	socksSuccess          byte = 0
	socksNotAllowed       byte = 2
	socksHostUnreachable  byte = 4
	socksCmdNotSupported  byte = 7
	socksAtypNotSupported byte = 8
)

const socksConnect = 1

// socksTarget is what a CONNECT request asked for. Host stays as the client
// wrote it -- a name is NOT resolved locally: the whole point is that the name
// is resolved at jmsd's end of the tunnel, where the internal DNS lives, and
// where the policy check can be applied to the answer.
type socksTarget struct {
	Host string
	Port int
}

func (t socksTarget) String() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

var (
	errUnsupportedCommand  = errors.New("only CONNECT is supported")
	errUnsupportedAddrType = errors.New("unsupported address type")
)

// socksHandshake performs the greeting and reads one CONNECT request. It
// writes the method reply itself; the caller writes the connect reply once it
// knows whether the stream opened, via socksReply.
func socksHandshake(rw io.ReadWriter) (socksTarget, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(rw, head); err != nil {
		return socksTarget{}, fmt.Errorf("read greeting: %w", err)
	}
	if head[0] != socksVersion {
		return socksTarget{}, fmt.Errorf("unsupported socks version %d", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(rw, methods); err != nil {
		return socksTarget{}, fmt.Errorf("read auth methods: %w", err)
	}
	// No authentication, because the listener is bound to loopback and the
	// real gate is the SSH key plus jmsd's policy. The ceiling that leaves:
	// any process running as any user on this machine can use the tunnel
	// while it is up. On a shared box, do not leave it running.
	//
	// ponytail: no SOCKS auth of any kind. Add RFC 1929 username/password if
	// this ever needs to be reachable from another machine.
	if _, err := rw.Write([]byte{socksVersion, 0}); err != nil {
		return socksTarget{}, fmt.Errorf("write auth reply: %w", err)
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(rw, req); err != nil {
		return socksTarget{}, fmt.Errorf("read request: %w", err)
	}
	if req[0] != socksVersion {
		return socksTarget{}, fmt.Errorf("unsupported socks version %d", req[0])
	}
	if req[1] != socksConnect {
		return socksTarget{}, errUnsupportedCommand
	}

	var host string
	switch req[3] {
	case 1: // IPv4
		buf := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(rw, buf); err != nil {
			return socksTarget{}, fmt.Errorf("read ipv4: %w", err)
		}
		host = net.IP(buf).String()
	case 3: // domain name
		n := make([]byte, 1)
		if _, err := io.ReadFull(rw, n); err != nil {
			return socksTarget{}, fmt.Errorf("read name length: %w", err)
		}
		buf := make([]byte, int(n[0]))
		if _, err := io.ReadFull(rw, buf); err != nil {
			return socksTarget{}, fmt.Errorf("read name: %w", err)
		}
		host = string(buf)
	case 4: // IPv6
		buf := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(rw, buf); err != nil {
			return socksTarget{}, fmt.Errorf("read ipv6: %w", err)
		}
		host = net.IP(buf).String()
	default:
		return socksTarget{}, errUnsupportedAddrType
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(rw, portBuf); err != nil {
		return socksTarget{}, fmt.Errorf("read port: %w", err)
	}
	return socksTarget{Host: host, Port: int(binary.BigEndian.Uint16(portBuf))}, nil
}

// socksReply writes a reply with a zeroed IPv4 bind address. The bind address
// is only meaningful for BIND, which is not supported, and clients ignore it
// for CONNECT.
func socksReply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{socksVersion, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

// socksFailureCode maps a stream-open failure to the reply a client can act
// on. "Not allowed by ruleset" is the honest answer for a policy refusal --
// curl prints it as such, which is far more use than a generic failure when
// the destination is simply not in tunnel.allow.
func socksFailureCode(err error) byte {
	switch {
	case errors.Is(err, errUnsupportedCommand):
		return socksCmdNotSupported
	case errors.Is(err, errUnsupportedAddrType):
		return socksAtypNotSupported
	case errors.Is(err, ErrDestinationRefused):
		return socksNotAllowed
	default:
		return socksHostUnreachable
	}
}
