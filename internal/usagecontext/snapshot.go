// Package usagecontext freezes bounded request metadata for asynchronous usage observers.
package usagecontext

import (
	"context"
	"net/http"
	"sort"
	"unicode/utf8"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Install configures the process-wide observer context adapter.
func Install() {
	coreusage.SetObserverContextSnapshotter(Snapshot)
}

// Snapshot copies only the existing bounded generic-observer context contract.
// CPAUK never receives this context; its inline tap builds a sanitized Event.
func Snapshot(source context.Context, limit int) (context.Context, int) {
	if source == nil || limit <= 0 {
		return context.Background(), 0
	}
	remaining := limit
	copyString := func(value string) string {
		if remaining <= 0 {
			return ""
		}
		value = truncate(value, remaining)
		remaining -= len(value)
		return value
	}

	requestID := copyString(internallogging.GetRequestID(source))
	endpoint := copyString(internallogging.GetEndpoint(source))
	metadata := internallogging.GetClientRequestMetadata(source)
	metadata.ClientIP = copyString(metadata.ClientIP)
	metadata.XForwardedFor = copyString(metadata.XForwardedFor)
	metadata.UserAgent = copyString(metadata.UserAgent)
	headers := snapshotHeaders(internallogging.GetResponseHeaders(source), &remaining)

	result := context.Background()
	result = internallogging.WithRequestID(result, requestID)
	result = internallogging.WithEndpoint(result, endpoint)
	result = internallogging.WithClientRequestMetadata(result, metadata)
	result = internallogging.WithResponseStatusHolder(result)
	internallogging.SetResponseStatus(result, internallogging.GetResponseStatus(source))
	result = internallogging.WithResponseHeadersHolder(result)
	internallogging.SetResponseHeaders(result, headers)
	return result, limit - remaining
}

func snapshotHeaders(source http.Header, remaining *int) http.Header {
	if len(source) == 0 || remaining == nil || *remaining <= 0 {
		return nil
	}
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 64 {
		keys = keys[:64]
	}
	result := make(http.Header)
	for _, key := range keys {
		if *remaining <= 64 {
			break
		}
		*remaining -= 64
		boundedKey := truncate(key, *remaining)
		*remaining -= len(boundedKey)
		if boundedKey == "" {
			break
		}
		values := source[key]
		if len(values) > 16 {
			values = values[:16]
		}
		for _, value := range values {
			if *remaining <= 16 {
				break
			}
			*remaining -= 16
			value = truncate(value, *remaining)
			*remaining -= len(value)
			result[boundedKey] = append(result[boundedKey], value)
		}
	}
	return result
}

func truncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
