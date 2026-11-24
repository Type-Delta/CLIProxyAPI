package usage

import "testing"

func TestDetailCloneDetachesCachedModalities(t *testing.T) {
	if clone := (Detail{}).Clone(); clone.CachedTextTokens != nil || clone.CachedImageTokens != nil || clone.CachedAudioTokens != nil {
		t.Fatal("clone filled absent cached modality counts")
	}
	text, image, audio := int64(3), int64(5), int64(7)
	original := Detail{CachedTextTokens: &text, CachedImageTokens: &image, CachedAudioTokens: &audio}
	clone := original.Clone()
	for _, tc := range []struct {
		name           string
		original, copy *int64
	}{
		{"text", original.CachedTextTokens, clone.CachedTextTokens},
		{"image", original.CachedImageTokens, clone.CachedImageTokens},
		{"audio", original.CachedAudioTokens, clone.CachedAudioTokens},
	} {
		if tc.copy == nil || tc.copy == tc.original || *tc.copy != *tc.original {
			t.Errorf("%s cached tokens were not independently cloned", tc.name)
			continue
		}
		*tc.copy += 1
		if *tc.copy == *tc.original {
			t.Errorf("changing cloned %s cached tokens changed the original", tc.name)
		}
	}
}
