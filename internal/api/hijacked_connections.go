package api

import (
	"bufio"
	"net"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

type ownedHijackedConn struct {
	net.Conn
	owner *Server
	once  sync.Once
	err   error
}

func (c *ownedHijackedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.hijackedMu.Lock()
		delete(c.owner.hijacked, c)
		c.owner.hijackedMu.Unlock()
	})
	return c.err
}

type ownedHijackWriter struct {
	gin.ResponseWriter
	owner *Server
}

// FlushError keeps transport failures visible through the ownership wrapper.
func (w *ownedHijackWriter) FlushError() error           { return httpwire.FlushResponse(w.ResponseWriter) }
func (w *ownedHijackWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *ownedHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.Hijack()
	if err != nil {
		return nil, nil, err
	}
	owned := &ownedHijackedConn{Conn: conn, owner: w.owner}
	w.owner.hijackedMu.Lock()
	stopping := w.owner.hijacksStopping
	if !stopping {
		if w.owner.hijacked == nil {
			w.owner.hijacked = make(map[net.Conn]struct{})
		}
		w.owner.hijacked[owned] = struct{}{}
	}
	w.owner.hijackedMu.Unlock()
	if stopping {
		_ = owned.Close()
	}
	return owned, rw, nil
}
func (s *Server) ownedHijacksMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) { c.Writer = &ownedHijackWriter{ResponseWriter: c.Writer, owner: s}; c.Next() }
}

// closeOwnedHijacks releases WebSocket readers before request capture finalizes.
// HTTP Server.Close deliberately does not close hijacked connections.
func (s *Server) closeOwnedHijacks() {
	s.hijackedMu.Lock()
	s.hijacksStopping = true
	conns := make([]net.Conn, 0, len(s.hijacked))
	for conn := range s.hijacked {
		conns = append(conns, conn)
	}
	s.hijackedMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}
