package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// FirstChunkStream is a streaming execution as seen by a handler after the first chunk,
// the first error, or a client disconnect.
type FirstChunkStream struct {
	Data    <-chan []byte
	Errors  <-chan *interfaces.ErrorMessage
	Headers http.Header

	// Committed reports that pre-first-chunk keep-alives already sent HTTP 200 with SSE
	// headers. Handlers must then skip their JSON error path and deliver every outcome,
	// including the first chunk and early failures, through their stream forwarder.
	// Upstream response headers can no longer reach the client in that case.
	Committed bool
}

type firstChunkStreamStart struct {
	data    <-chan []byte
	headers http.Header
	errs    <-chan *interfaces.ErrorMessage
}

// ExecuteStreamWithFirstChunkKeepAlive wraps ExecuteStreamWithAuthManager for SSE handlers so
// the client keeps receiving bytes while credential selection, cooldown waits, retries and the
// upstream's time to first chunk are still pending. Proxies in front of CPA (nginx
// proxy_read_timeout, Cloudflare's 125s origin timeout) otherwise cut silent requests.
//
// It reuses streaming.keepalive-seconds. Nothing is written until one interval passes without
// a first chunk or error, so fast failures keep their real HTTP status. After that it calls
// setSSEHeaders, commits HTTP 200, and writes ": keep-alive" every interval until the first
// chunk or error arrives. A nil setSSEHeaders, a disabled interval, or a writer without
// Flush keeps the previous behavior.
//
// ctx must be the execution context passed to ExecuteStreamWithAuthManager; cancelling it
// stops the background relay. Data yields the first chunk followed by the rest unchanged, and
// Errors still carries the first error if one ended the wait.
func (h *BaseAPIHandler) ExecuteStreamWithFirstChunkKeepAlive(c *gin.Context, ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, setSSEHeaders func()) FirstChunkStream {
	interval := StreamingKeepAliveInterval(h.Cfg)
	flusher, canFlush := c.Writer.(http.Flusher)
	if interval <= 0 || !canFlush || setSSEHeaders == nil {
		data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt)
		return FirstChunkStream{Data: data, Errors: errs, Headers: headers}
	}

	started := make(chan firstChunkStreamStart, 1)
	go func() {
		data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt)
		started <- firstChunkStreamStart{data: data, headers: headers, errs: errs}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	committed := false
	keepAlive := func() {
		if !committed {
			setSSEHeaders()
			committed = true
		}
		_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		flusher.Flush()
	}
	disconnected := func() FirstChunkStream {
		// The handler's own request-context check ends the request.
		return FirstChunkStream{Data: closedChunkChannel(), Errors: closedErrorChannel(), Committed: committed}
	}

	var start firstChunkStreamStart
waitStart:
	for {
		select {
		case start = <-started:
			break waitStart
		case <-c.Request.Context().Done():
			return disconnected()
		case <-ticker.C:
			keepAlive()
		}
	}

	data, errs := start.data, start.errs
	if data == nil {
		data = closedChunkChannel()
	}
	for {
		select {
		case <-c.Request.Context().Done():
			return disconnected()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// Re-deliver the error so the handler's existing first-outcome logic sees it.
			pending := make(chan *interfaces.ErrorMessage, 1)
			pending <- errMsg
			close(pending)
			return FirstChunkStream{Data: data, Errors: pending, Headers: start.headers, Committed: committed}
		case chunk, ok := <-data:
			if errs == nil {
				errs = closedErrorChannel()
			}
			if !ok {
				return FirstChunkStream{Data: closedChunkChannel(), Errors: errs, Headers: start.headers, Committed: committed}
			}
			return FirstChunkStream{Data: prependChunk(ctx, chunk, data), Errors: errs, Headers: start.headers, Committed: committed}
		case <-ticker.C:
			keepAlive()
		}
	}
}

// prependChunk yields first and then everything from rest. It stops when ctx is done so an
// abandoned consumer does not leak the relay.
func prependChunk(ctx context.Context, first []byte, rest <-chan []byte) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		chunk, ok := first, true
		for ok {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
			chunk, ok = <-rest
		}
	}()
	return out
}

func closedChunkChannel() <-chan []byte {
	ch := make(chan []byte)
	close(ch)
	return ch
}

func closedErrorChannel() <-chan *interfaces.ErrorMessage {
	ch := make(chan *interfaces.ErrorMessage)
	close(ch)
	return ch
}
