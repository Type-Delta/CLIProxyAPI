package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// observeLiveResponsesTools counts output items, including hosted calls and calls
// emitted for the client to execute. It keeps only identities for deduplication.
func (r *UsageReporter) observeLiveResponsesTools(payload []byte) {
	if r.liveAttempt == nil {
		return
	}
	switch ojson.GetBytesNoCopy(payload, "type").String() {
	case "response.output_item.added", "response.output_item.done":
		slot := int64(-1)
		if index := ojson.GetBytesNoCopy(payload, "output_index"); index.Type == ojson.Number && index.Int() >= 0 {
			slot = index.Int()
		}
		r.recordLiveResponsesTool(ojson.GetBytesNoCopy(payload, "item"), slot, false)
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		for index, item := range ojson.GetBytesNoCopy(payload, "response.output").Array() {
			r.recordLiveResponsesTool(item, int64(index), true)
		}
	}
}

func (r *UsageReporter) recordLiveResponsesTool(item ojson.Result, slot int64, terminal bool) {
	kind := item.Get("type").String()
	if !strings.HasSuffix(kind, "_call") {
		return
	}
	id := item.Get("id").String()
	if id == "" {
		id = item.Get("call_id").String()
	}
	if slot < 0 && id == "" {
		return
	}
	r.ttftMu.Lock()
	defer r.ttftMu.Unlock()
	_, seenID := r.liveToolIDs[id]
	previousID, seenSlot := r.liveToolSlots[slot]
	// Terminal output can omit earlier items and shift later slots. Prefer IDs
	// when both frames supply them.
	seen := seenID || seenSlot && (id == "" || previousID == "" || !terminal)
	if id != "" && !seenID {
		if r.liveToolIDs == nil {
			r.liveToolIDs = make(map[string]struct{})
		}
		r.liveToolIDs[strings.Clone(id)] = struct{}{}
	}
	if slot >= 0 && (!seenSlot || previousID == "") {
		if r.liveToolSlots == nil {
			r.liveToolSlots = make(map[int64]string)
		}
		r.liveToolSlots[slot] = strings.Clone(id)
	}
	if !seen {
		r.liveToolCount++
		r.liveAttempt.SetToolCalls(r.liveToolCount)
	}
}
