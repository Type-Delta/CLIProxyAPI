package api

import (
	"bufio"
	"crypto/tls"
	"net"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c == nil {
		return 0, net.ErrClosed
	}
	if c.reader == nil {
		return c.Conn.Read(p)
	}
	return c.reader.Read(p)
}

// bufferedTLSConn preserves TLS state after protocol sniffing consumes bytes
// into a buffered reader. net/http uses ConnectionState to populate Request.TLS.
type bufferedTLSConn struct {
	*bufferedConn
	tlsConn *tls.Conn
}

func (c *bufferedTLSConn) ConnectionState() tls.ConnectionState {
	if c == nil || c.tlsConn == nil {
		return tls.ConnectionState{}
	}
	return c.tlsConn.ConnectionState()
}
