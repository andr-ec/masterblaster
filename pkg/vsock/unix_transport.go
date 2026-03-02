package vsock

import (
	"fmt"
	"net"
	"time"
)

// UnixTransport connects to stereosd via a Unix domain socket. This is used
// when masterblaster runs natively on the same host as stereosd (no VM).
type UnixTransport struct {
	Path string
}

// Dial connects to stereosd via the Unix socket.
func (t *UnixTransport) Dial(timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", t.Path, timeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to stereosd at %s: %w", t.Path, err)
	}
	return conn, nil
}

// String returns the Unix socket path.
func (t *UnixTransport) String() string {
	return fmt.Sprintf("unix:%s", t.Path)
}
