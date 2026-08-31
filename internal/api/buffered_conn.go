package api

import (
	"bufio"
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
