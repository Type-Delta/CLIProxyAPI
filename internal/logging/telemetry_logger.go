package logging

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	log "github.com/sirupsen/logrus"
)

// TelemetryRequestLogger publishes structured request records to the outbox.
type TelemetryRequestLogger struct {
	enabled atomic.Bool
	dir     string
}

func NewTelemetryRequestLogger(enabled bool, dir, configDir string) *TelemetryRequestLogger {
	if !filepath.IsAbs(dir) && configDir != "" {
		dir = filepath.Join(configDir, dir)
	}
	if count, size, err := outbox.CleanupTemps(dir, time.Now()); err != nil {
		log.WithError(err).Warn("telemetry: failed to reclaim abandoned captures")
	} else if count > 0 {
		log.WithFields(log.Fields{"files": count, "bytes": size}).Warnf("telemetry: reclaimed %d file(s), %d bytes of abandoned captures", count, size)
	}
	l := &TelemetryRequestLogger{dir: dir}
	l.enabled.Store(enabled)
	return l
}
func (l *TelemetryRequestLogger) IsEnabled() bool         { return l.enabled.Load() }
func (l *TelemetryRequestLogger) SetEnabled(enabled bool) { l.enabled.Store(enabled) }
func (l *TelemetryRequestLogger) NewFileBodySource(prefix string) (*FileBodySource, error) {
	return NewFileBodySourceInDir(l.dir, prefix)
}
func (l *TelemetryRequestLogger) NewCapture() *TelemetryCapture       { return NewTelemetryCapture(l.dir) }
func (l *TelemetryRequestLogger) WriteRecord(r *record.Request) error { return outbox.Write(l.dir, r) }
func (l *TelemetryRequestLogger) LogRequest(url, method string, headers map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, timeline, apiRequest, apiResponse, apiTimeline []byte, errs []*interfaces.ErrorMessage, id string, started, responseAt time.Time) error {
	// Middleware owns the complete record, including attempts and error-only capture.
	return nil
}
func (l *TelemetryRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, id string) (StreamingLogWriter, error) {
	source, err := l.NewFileBodySource("telemetry-stream")
	if err != nil {
		return nil, err
	}
	return &TelemetryStreamingWriter{source: source, logger: l, request: record.Request{RequestID: id, StartedAt: time.Now(), Method: method, URL: MaskURL(url), Headers: MaskHeaders(headers), Body: body, Streaming: true, Detail: record.DetailFull, Source: record.SourceLive, Transport: record.TransportHTTP}}, nil
}

// TelemetryStreamingWriter spools downstream chunks before final record publication.
type TelemetryStreamingWriter struct {
	source    *FileBodySource
	logger    *TelemetryRequestLogger
	request   record.Request
	truncated atomic.Bool
}

func (w *TelemetryStreamingWriter) WriteChunkAsync(chunk []byte) {
	if err := w.source.AppendBytes(chunk); err != nil {
		w.truncated.Store(true)
	}
}
func (w *TelemetryStreamingWriter) WriteStatus(status int, headers map[string][]string) error {
	w.request.Status = status
	w.request.ResponseHeaders = MaskHeaders(headers)
	return nil
}
func (w *TelemetryStreamingWriter) WriteAPIRequest([]byte) error           { return nil }
func (w *TelemetryStreamingWriter) WriteAPIResponse([]byte) error          { return nil }
func (w *TelemetryStreamingWriter) WriteAPIWebsocketTimeline([]byte) error { return nil }
func (w *TelemetryStreamingWriter) SetFirstChunkTimestamp(t time.Time)     { w.request.FirstChunkAt = t }
func (w *TelemetryStreamingWriter) Close() error {
	// Close only releases resources. Finish supplies bytes to middleware, the sole publisher.
	if w.source != nil {
		return w.source.Cleanup()
	}
	return nil
}
func (w *TelemetryStreamingWriter) DroppedChunk() { w.truncated.Store(true) }
func (w *TelemetryStreamingWriter) Finish() ([]byte, bool, error) {
	if w.source == nil {
		return nil, true, fmt.Errorf("missing stream source")
	}
	b, err := w.source.Bytes()
	cleanupErr := w.source.Cleanup()
	if err == nil {
		err = cleanupErr
	}
	return b, w.truncated.Load() || err != nil, err
}

// DecompressTelemetryResponse uses the same decoding as file logs.
func DecompressTelemetryResponse(headers map[string][]string, body []byte) []byte {
	decoded, err := (&FileRequestLogger{}).decompressResponse(headers, body)
	if err != nil {
		return body
	}
	return decoded
}
