package usage

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MaxProxyErrorBytes bounds ProxyResponse.Error.
const MaxProxyErrorBytes = 2048

// ProxyResponse describes what the proxy finally returned to the downstream
// client for one proxy request. It is published once the handler finishes.
type ProxyResponse struct {
	ProxyRequestID string
	StatusCode     int
	Error          string
	RespondedAt    time.Time
}

// ProxyResponseObserver receives finalized downstream responses. It must not block.
type ProxyResponseObserver func(ProxyResponse)

var proxyResponseObserver struct {
	sync.RWMutex
	fn ProxyResponseObserver
}

// SetProxyResponseObserver installs the process-wide observer; nil detaches it.
func SetProxyResponseObserver(observer ProxyResponseObserver) {
	proxyResponseObserver.Lock()
	proxyResponseObserver.fn = observer
	proxyResponseObserver.Unlock()
}

// PublishProxyResponse delivers the downstream response outcome to the observer.
// The proxy request ID comes from ctx when the caller did not set it; responses
// without a valid ID are dropped because they cannot be correlated.
func PublishProxyResponse(ctx context.Context, response ProxyResponse) {
	response.ProxyRequestID = strings.TrimSpace(response.ProxyRequestID)
	if !ValidProxyRequestID(response.ProxyRequestID) {
		response.ProxyRequestID = ProxyRequestIDFromContext(ctx)
	}
	if response.ProxyRequestID == "" {
		return
	}
	response.Error = truncateUTF8(strings.TrimSpace(response.Error), MaxProxyErrorBytes)
	if response.RespondedAt.IsZero() {
		response.RespondedAt = time.Now()
	}
	proxyResponseObserver.RLock()
	observer := proxyResponseObserver.fn
	proxyResponseObserver.RUnlock()
	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer(response)
}
