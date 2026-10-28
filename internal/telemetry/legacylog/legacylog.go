// Package legacylog imports old text request logs without changing them.
package legacylog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

type section struct{ name, text string }

// Parse reads the ordered sections of the legacy log format. Because the format
// has no length framing, payloads containing section delimiters are ambiguous.
func Parse(name string, data []byte) (*record.Request, error) {
	s := string(data)
	if !strings.HasPrefix(s, "=== REQUEST INFO ===\n") {
		return nil, fmt.Errorf("missing request info")
	}
	lines := strings.Split(s, "\n")
	var sections []section
	var current *section
	var text strings.Builder
	responseLine := -1
	for i, line := range lines {
		if line == "=== RESPONSE ===" && i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "Status: ") || strings.Contains(lines[i+1], ": ") || lines[i+1] == "") {
			responseLine = i
		}
	}
	for i, line := range lines {
		marker := line == "=== REQUEST INFO ===" && i == 0
		if i > 0 && (responseLine < 0 || i <= responseLine) && isMarker(line) {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "=== "), " ===")
			switch {
			case name == "HEADERS":
				marker = len(sections) == 1
			case name == "REQUEST BODY":
				marker = len(sections) == 2
			case name == "RESPONSE":
				marker = i == responseLine
			case strings.HasPrefix(name, "API REQUEST"):
				marker = i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "Timestamp: ") || lines[i+1] == "<missing>")
			case strings.HasPrefix(name, "API RESPONSE"):
				marker = i+1 < len(lines) && (strings.HasPrefix(lines[i+1], "Timestamp: ") || strings.HasPrefix(lines[i+1], "Status: ") || lines[i+1] == "Headers:")
			case name == "API ERROR RESPONSE":
				marker = i+1 < len(lines) && strings.HasPrefix(lines[i+1], "HTTP Status: ")
			case name == "WEBSOCKET TIMELINE", name == "API WEBSOCKET TIMELINE":
				marker = true
			}
		}
		if marker {
			if current != nil {
				current.text = text.String()
				text = strings.Builder{}
			}
			sections = append(sections, section{name: strings.TrimSuffix(strings.TrimPrefix(line, "=== "), " ===")})
			current = &sections[len(sections)-1]
			continue
		}
		if current != nil {
			text.WriteString(line)
			text.WriteByte('\n')
		}
	}
	if current != nil {
		current.text = text.String()
	}
	if len(sections) < 2 || sections[0].name != "REQUEST INFO" || sections[1].name != "HEADERS" {
		return nil, fmt.Errorf("invalid section order")
	}
	hash := sha256.Sum256([]byte(sourceIdentity(name)))
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(name), ".gz"), ".log")
	shortID := base[strings.LastIndex(base, "-")+1:]
	// Legacy files carry only the last eight characters of the request-log ID.
	// The ID keeps them as its suffix so lookups by that short ID still match;
	// ProxyRequestID stays empty because the analytics join key was never logged.
	r := &record.Request{RequestID: "legacy-" + hex.EncodeToString(hash[:16]) + "-" + shortID, Source: record.SourceLegacyLog, Detail: record.DetailFull, Transport: record.TransportHTTP, Headers: map[string][]string{}}
	if strings.HasPrefix(filepath.Base(name), "error-") {
		r.Detail = record.DetailError
	}
	info := keyValues(sections[0].text)
	r.URL = first(info, "URL")
	r.Method = first(info, "Method")
	r.Transport = first(info, "Downstream Transport")
	if r.Transport == "" {
		r.Transport = record.TransportHTTP
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, first(info, "Timestamp"))
	if r.StartedAt.IsZero() || r.URL == "" || r.Method == "" {
		return nil, fmt.Errorf("missing request metadata")
	}
	r.Headers = keyValues(sections[1].text)
	attempts := map[int]*record.Attempt{}
	for _, sec := range sections[2:] {
		text := strings.TrimSuffix(strings.TrimSuffix(sec.text, "\n"), "\n")
		if sec.name == "RESPONSE" {
			text = strings.TrimSuffix(sec.text, "\n")
		}
		switch {
		case sec.name == "REQUEST BODY":
			r.Body = []byte(strings.TrimSuffix(text, "\n"))
		case sec.name == "WEBSOCKET TIMELINE":
			r.WebsocketTimeline = []byte(text)
		case sec.name == "API WEBSOCKET TIMELINE":
			if attempts[1] == nil {
				attempts[1] = &record.Attempt{Index: 1}
			}
			attempts[1].WebsocketTimeline = []byte(text)
		case strings.HasPrefix(sec.name, "API REQUEST"):
			idx := sectionIndex(sec.name)
			if attempts[idx] == nil {
				attempts[idx] = &record.Attempt{Index: idx}
			}
			a := attempts[idx]
			meta, headers, body := parseAPI(text)
			a.URL = first(meta, "Upstream URL")
			a.Method = first(meta, "HTTP Method")
			a.StartedAt, _ = time.Parse(time.RFC3339Nano, first(meta, "Timestamp"))
			a.Headers = headers
			a.Body = []byte(body)
		case strings.HasPrefix(sec.name, "API RESPONSE"):
			idx := sectionIndex(sec.name)
			if attempts[idx] == nil {
				attempts[idx] = &record.Attempt{Index: idx}
			}
			a := attempts[idx]
			meta, headers, body := parseAPI(text)
			a.Status, _ = strconv.Atoi(first(meta, "Status"))
			a.ResponseAt, _ = time.Parse(time.RFC3339Nano, first(meta, "Timestamp"))
			a.ResponseHeaders = headers
			a.Errors = append(a.Errors, meta["Error"]...)
			a.Errors = append(a.Errors, headers["Error"]...)
			delete(a.ResponseHeaders, "Error")
			// The helper may append an error directly after a response chunk.
			// Preserve the whole response because this marker is unframed.
			if pos := strings.LastIndex(body, "Error: "); pos >= 0 {
				a.Errors = append(a.Errors, strings.TrimSpace(body[pos+len("Error: "):]))
			}
			a.Response = []byte(body)
		case sec.name == "API ERROR RESPONSE":
			r.Errors = append(r.Errors, text)
			if len(attempts) > 0 {
				last := 0
				for n := range attempts {
					if n > last {
						last = n
					}
				}
				attempts[last].Errors = append(attempts[last].Errors, text)
			}
		case sec.name == "RESPONSE":
			head, body, _ := strings.Cut(text, "\n\n")
			h := keyValues(head)
			r.Status, _ = strconv.Atoi(first(h, "Status"))
			delete(h, "Status")
			r.ResponseHeaders = h
			r.Streaming = strings.Contains(strings.ToLower(first(h, "Content-Type")), "text/event-stream")
			if !r.Streaming {
				body = strings.TrimSuffix(body, "\n")
			}
			r.Response = []byte(body)
		default:
			return nil, fmt.Errorf("unrecognized or ambiguous section %q", sec.name)
		}
	}
	if responseLine < 0 && len(r.WebsocketTimeline) == 0 {
		return nil, fmt.Errorf("missing response or websocket timeline")
	}
	keys := make([]int, 0, len(attempts))
	for n := range attempts {
		keys = append(keys, n)
	}
	sort.Ints(keys)
	for _, n := range keys {
		r.Attempts = append(r.Attempts, *attempts[n])
	}
	return r, nil
}
func keyValues(s string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			m[strings.TrimSpace(k)] = append(m[strings.TrimSpace(k)], strings.TrimSpace(v))
		}
	}
	return m
}
func first(m map[string][]string, k string) string {
	for key, v := range m {
		if strings.EqualFold(key, k) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
func sectionIndex(s string) int {
	fields := strings.Fields(s)
	if len(fields) > 2 {
		if n, e := strconv.Atoi(fields[len(fields)-1]); e == nil && n > 0 {
			return n
		}
	}
	return 1
}
func parseAPI(s string) (map[string][]string, map[string][]string, string) {
	head, body, ok := strings.Cut(s, "\nBody:\n")
	if !ok {
		head = s
	}
	meta, headers, ok := strings.Cut(head, "\nHeaders:\n")
	if !ok {
		meta = head
	}
	if strings.TrimSpace(body) == "<empty>" {
		body = ""
	}
	return keyValues(meta), keyValues(headers), strings.TrimSuffix(body, "\n")
}

// Only recognize section names emitted by the logger. Arbitrary marker-like
// lines in raw bodies are otherwise retained as payload.
func isMarker(line string) bool {
	if !strings.HasPrefix(line, "=== ") || !strings.HasSuffix(line, " ===") {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(line, "=== "), " ===")
	switch name {
	case "REQUEST BODY", "HEADERS", "WEBSOCKET TIMELINE", "API WEBSOCKET TIMELINE", "API ERROR RESPONSE", "RESPONSE":
		return true
	}
	for _, prefix := range []string{"API REQUEST", "API RESPONSE"} {
		if name == prefix {
			return true
		}
		if strings.HasPrefix(name, prefix+" ") {
			n, err := strconv.Atoi(strings.TrimPrefix(name, prefix+" "))
			return err == nil && n > 0
		}
	}
	return false
}

// Compression rotation does not create a new legacy request or sort key.
func sourceIdentity(name string) string {
	if name == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(name), ".gz")
}
