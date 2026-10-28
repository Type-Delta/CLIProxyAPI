package logging

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

// RenderRequestLog renders a captured record using the legacy request log sections.
func RenderRequestLog(r *record.Request) []byte {
	if r == nil {
		return nil
	}
	var requests, responses, timeline bytes.Buffer
	for _, a := range r.Attempts {
		fmt.Fprintf(&requests, "=== API REQUEST %d ===\nTimestamp: %s\n", a.Index, a.StartedAt.Format(time.RFC3339Nano))
		if a.URL != "" {
			fmt.Fprintf(&requests, "Upstream URL: %s\n", a.URL)
		} else {
			requests.WriteString("Upstream URL: <unknown>\n")
		}
		if a.Method != "" {
			fmt.Fprintf(&requests, "HTTP Method: %s\n", a.Method)
		}
		var auth []string
		if a.Provider != "" {
			auth = append(auth, "provider="+a.Provider)
		}
		if a.AuthID != "" {
			auth = append(auth, "auth_id="+a.AuthID)
		}
		if a.AuthLabel != "" {
			auth = append(auth, "label="+a.AuthLabel)
		}
		if a.AuthType != "" {
			auth = append(auth, "type="+a.AuthType)
		}
		if len(auth) > 0 {
			fmt.Fprintf(&requests, "Auth: %s\n", strings.Join(auth, ", "))
		}
		requests.WriteString("\nHeaders:\n")
		renderAttemptHeaders(&requests, a.Headers)
		requests.WriteString("\nBody:\n")
		if len(a.Body) == 0 {
			requests.WriteString("<empty>")
		} else {
			requests.Write(a.Body)
		}
		requests.WriteString("\n\n")
		if a.Status != 0 || len(a.ResponseHeaders) > 0 || len(a.Response) > 0 || len(a.Errors) > 0 {
			fmt.Fprintf(&responses, "=== API RESPONSE %d ===\nTimestamp: %s\n\n", a.Index, a.ResponseAt.Format(time.RFC3339Nano))
			if a.Status != 0 {
				fmt.Fprintf(&responses, "Status: %d\n", a.Status)
			}
			responses.WriteString("Headers:\n")
			renderAttemptHeaders(&responses, a.ResponseHeaders)
			responses.WriteString("\n")
			for _, message := range a.Errors {
				fmt.Fprintf(&responses, "Error: %s\n", message)
			}
			if len(a.Response) > 0 {
				responses.WriteString("Body:\n")
				responses.Write(bytes.TrimSpace(a.Response))
				responses.WriteString("\n")
			}
			responses.WriteString("\n")
		}
		timeline.Write(a.WebsocketTimeline)
	}
	for _, message := range r.Errors {
		requests.WriteString("=== API ERROR RESPONSE ===\n")
		if !strings.HasPrefix(message, "HTTP Status: ") {
			fmt.Fprintf(&requests, "HTTP Status: %d\n", r.Status)
		}
		requests.WriteString(message)
		requests.WriteString("\n\n")
	}
	var result bytes.Buffer
	_ = (&FileRequestLogger{}).writeNonStreamingLog(&result, r.URL, r.Method, r.Headers, r.Body, "", r.WebsocketTimeline, nil, requests.Bytes(), nil, responses.Bytes(), nil, timeline.Bytes(), nil, nil, r.Status, r.ResponseHeaders, r.Response, nil, r.StartedAt, time.Time{})
	return result.Bytes()
}
func renderAttemptHeaders(buf *bytes.Buffer, headers map[string][]string) {
	if len(headers) == 0 {
		buf.WriteString("<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(headers[key]) == 0 {
			fmt.Fprintf(buf, "%s:\n", key)
		}
		for _, value := range headers[key] {
			fmt.Fprintf(buf, "%s: %s\n", key, value)
		}
	}
}
