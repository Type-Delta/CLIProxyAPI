package chstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// A resumed run recounts its range, so intake between runs is reflected.
func TestS3RepriceResumeRecountsAfterIntake(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := base.RequestedAt
	events := []model.Event{base, base}
	for j := range events {
		events[j].AttemptID = fmt.Sprintf("%032x", j+100)
		events[j].ProxyRequestID = events[j].AttemptID
		events[j].RequestedAt = start.Add(time.Duration(j) * time.Second)
	}
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	options := RepriceOptions{Range: model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}, ChunkSize: 1}
	first, err := s.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	added := base
	added.AttemptID = fmt.Sprintf("%032x", 102)
	added.ProxyRequestID = added.AttemptID
	added.RequestedAt = start.Add(2 * time.Second)
	if err := s.WriteBatch(ctx, []model.Event{added}); err != nil {
		t.Fatal(err)
	}
	options.ResumeCheckpoint = first.Checkpoint
	second, err := s.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Matched != 2 || second.Matched != 3 {
		t.Fatalf("matched first=%d resumed=%d, want 2 and 3", first.Matched, second.Matched)
	}
}
