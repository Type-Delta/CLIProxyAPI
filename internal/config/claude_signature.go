package config

import (
	"encoding/json"
)

// UnmarshalJSON accepts the old spelling for SDK and management clients while
// serialization uses only the canonical signature-stabilization field tag.
func (hd *ClaudeHeaderDefaults) UnmarshalJSON(data []byte) error {
	type wire ClaudeHeaderDefaults
	decoded := wire(*hd)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, canonical := fields["signature-stabilization"]; !canonical {
		if legacy, exists := fields["stabilize-device-profile"]; exists {
			if err := json.Unmarshal(legacy, &decoded.StabilizeDeviceProfile); err != nil {
				return err
			}
		}
	}
	*hd = ClaudeHeaderDefaults(decoded)
	return nil
}
