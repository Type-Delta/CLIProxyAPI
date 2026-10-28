package logging

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const TelemetryCaptureContextKey = "TELEMETRY_CAPTURE"

// TelemetryCapture holds upstream data until the client request finishes.
type TelemetryCapture struct {
	mu        sync.Mutex
	dir       string
	attempts  []record.Attempt
	responses []*FileBodySource
	timelines []*FileBodySource
	secrets   []string
}

func NewTelemetryCapture(dir string) *TelemetryCapture { return &TelemetryCapture{dir: dir} }

func MaskHeaders(headers map[string][]string) map[string][]string {
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(name, value)
			if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Set-Cookie") {
				masked = "[REDACTED]"
			}
			result[name] = append(result[name], masked)
		}
	}
	return result
}

func MaskURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.RawQuery = util.MaskSensitiveQuery(parsed.RawQuery)
	parsed.User = nil
	return parsed.String()
}

// AddSecret keeps known credentials in memory only, for redacting error messages.
func (c *TelemetryCapture) AddSecret(secret string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secrets = append(c.secrets, secret)
}
func (c *TelemetryCapture) redact(message string) string {
	for _, secret := range c.secrets {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}
func (c *TelemetryCapture) Redact(message string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redact(message)
}

func (c *TelemetryCapture) StartAttempt(a record.Attempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if parsed, err := url.Parse(a.URL); err == nil {
		for key, values := range parsed.Query() {
			for _, value := range values {
				pair := url.QueryEscape(key) + "=" + url.QueryEscape(value)
				if value != "" && util.MaskSensitiveQuery(pair) != pair {
					c.secrets = append(c.secrets, value)
				}
			}
		}
	}
	for key, values := range a.Headers {
		for _, value := range values {
			if value != "" && (util.MaskSensitiveHeaderValue(key, value) != value || strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "Set-Cookie")) {
				c.secrets = append(c.secrets, value)
				parts := strings.Fields(value)
				if len(parts) > 1 {
					c.secrets = append(c.secrets, parts[len(parts)-1])
				}
			}
		}
	}
	a.Index = len(c.attempts) + 1
	a.StartedAt = time.Now()
	a.URL = MaskURL(a.URL)
	a.Headers = MaskHeaders(a.Headers)
	a.Body = bytes.Clone(a.Body)
	c.attempts = append(c.attempts, a)
	c.responses = append(c.responses, nil)
	c.timelines = append(c.timelines, nil)
}
func (c *TelemetryCapture) Metadata(status int, headers http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.attempts) == 0 {
		return
	}
	a := &c.attempts[len(c.attempts)-1]
	if a.ResponseAt.IsZero() {
		a.ResponseAt = time.Now()
	}
	a.Status = status
	a.ResponseHeaders = MaskHeaders(headers)
}
func (c *TelemetryCapture) Chunk(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.attempts) == 0 {
		return
	}
	i := len(c.attempts) - 1
	if c.attempts[i].ResponseAt.IsZero() {
		c.attempts[i].ResponseAt = time.Now()
	}
	if len(chunk) == 0 {
		return
	}
	if c.responses[i] == nil {
		c.responses[i], _ = NewFileBodySourceInDir(c.dir, "telemetry-attempt")
	}
	if c.responses[i] == nil || c.responses[i].AppendBytes(chunk) != nil {
		c.attempts[i].ResponseTruncated = true
	}
}
func (c *TelemetryCapture) Error(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.attempts) > 0 {
		i := len(c.attempts) - 1
		if c.attempts[i].ResponseAt.IsZero() {
			c.attempts[i].ResponseAt = time.Now()
		}
		c.attempts[i].Errors = append(c.attempts[i].Errors, err.Error())
	}
}
func (c *TelemetryCapture) Timeline(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.attempts) == 0 {
		return
	}
	i := len(c.attempts) - 1
	if len(data) == 0 {
		return
	}
	if c.timelines[i] == nil {
		c.timelines[i], _ = NewFileBodySourceInDir(c.dir, "telemetry-timeline")
	}
	if c.timelines[i] == nil || c.timelines[i].AppendBytes(data) != nil {
		c.attempts[i].ResponseTruncated = true
	}
}

func (c *TelemetryCapture) Finish() []record.Attempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.attempts {
		for j := range c.attempts[i].Errors {
			c.attempts[i].Errors[j] = c.redact(c.attempts[i].Errors[j])
		}
	}
	for i, source := range c.responses {
		if source != nil {
			body, err := source.Bytes()
			if err != nil {
				c.attempts[i].ResponseTruncated = true
			} else {
				c.attempts[i].Response = body
			}
			_ = source.Cleanup()
		}
	}
	for i, source := range c.timelines {
		if source != nil {
			body, err := source.Bytes()
			if err != nil {
				c.attempts[i].ResponseTruncated = true
			} else {
				c.attempts[i].WebsocketTimeline = body
			}
			_ = source.Cleanup()
		}
	}
	c.responses = nil
	c.timelines = nil
	return c.attempts
}
