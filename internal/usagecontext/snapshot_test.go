package usagecontext

import (
	"context"
	"strings"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestSnapshotBoundsSessionMetadata(t *testing.T) {
	const limit = 128
	source := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		SessionID:       strings.Repeat("s", 1<<20),
		ParentSessionID: strings.Repeat("p", 1<<20),
	})

	snapshot, used := Snapshot(source, limit)
	metadata := internallogging.GetClientRequestMetadata(snapshot)
	gotBytes := len(metadata.ClientIP) + len(metadata.XForwardedFor) + len(metadata.UserAgent) +
		len(metadata.SessionID) + len(metadata.ParentSessionID)
	if gotBytes > limit {
		t.Fatalf("snapshot metadata uses %d bytes, maximum %d", gotBytes, limit)
	}
	if used != gotBytes {
		t.Fatalf("snapshot reports %d bytes for %d copied metadata bytes", used, gotBytes)
	}
}
