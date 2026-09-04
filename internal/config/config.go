package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const defaultSSHPort = 2222

// Endpoints is what jms actually dials. Only Koko's SSH port is left: Core is
// reached through jmsd, which holds its own base URL, so the other URLs this
// used to derive ended up with no caller.
type Endpoints struct {
	KokoAddr string
}

// ResolveServer turns the --server value into Koko's SSH address.
//
// It accepts a bare host as readily as a URL, since that is what people type.
// A URL's scheme is parsed but discarded: the only thing derived from --server
// is an SSH address, and SSH has no scheme. (There used to be a --scheme flag
// feeding this. It was validated and then thrown away with everything else --
// setting it changed nothing at all -- so it is gone.)
func ResolveServer(raw string, sshPort int) (Endpoints, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoints{}, errors.New("server address is required")
	}
	if sshPort == 0 {
		sshPort = defaultSSHPort
	}

	parseValue := raw
	if !strings.Contains(parseValue, "://") {
		// url.Parse needs one to recognise a host; which one is immaterial.
		parseValue = "https://" + parseValue
	}
	u, err := url.Parse(parseValue)
	if err != nil || u.Hostname() == "" {
		return Endpoints{}, fmt.Errorf("invalid server address %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return Endpoints{}, errors.New("server must be a host, not a URL path")
	}
	return Endpoints{
		KokoAddr: net.JoinHostPort(u.Hostname(), strconv.Itoa(sshPort)),
	}, nil
}
