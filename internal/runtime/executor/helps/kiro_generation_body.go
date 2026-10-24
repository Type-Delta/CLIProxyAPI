package helps

import (
	"context"
	"encoding/binary"
	"io"
	"time"
)

// TrackKiroGenerationBody observes complete binary frames at their source-read
// arrival. decode validates the existing wire format and returns new output only.
func (r *UsageReporter) TrackKiroGenerationBody(ctx context.Context, body io.ReadCloser, maxFrameBytes int, decode func([]byte) (string, error)) io.ReadCloser {
	if r == nil || body == nil {
		return body
	}
	r.SetOutputTimingSource("kiro", "source_reader", "wire_deltas")
	var pending []byte
	invalid := false
	return r.TrackGenerationBodyObserver(ctx, body, func(data []byte, at time.Time, readErr error) {
		if invalid {
			return
		}
		pending = append(pending, data...)
		for len(pending) >= 12 {
			length := int(binary.BigEndian.Uint32(pending[:4]))
			if length < 16 || length > maxFrameBytes {
				r.InvalidateGenerationTimingReason("frame_limit")
				invalid, pending = true, nil
				return
			}
			if len(pending) < length {
				break
			}
			text, err := decode(pending[:length])
			if err != nil {
				r.InvalidateGenerationTimingReason("malformed_stream")
				invalid, pending = true, nil
				return
			}
			r.ObserveOutputDeltaAt(text, at)
			pending = pending[length:]
		}
		if readErr != nil && len(pending) > 0 {
			r.InvalidateGenerationTimingReason("malformed_stream")
			invalid, pending = true, nil
		}
	})
}
