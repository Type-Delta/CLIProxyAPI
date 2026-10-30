package chstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *Store) Reprice(ctx context.Context, options RepriceOptions, progress func(int, string)) (RepriceResult, error) {
	if options.Range.Start.IsZero() || options.Range.End.IsZero() || !options.Range.Start.Before(options.Range.End) {
		return RepriceResult{}, fmt.Errorf("invalid reprice range")
	}
	if options.ChunkSize == 0 {
		options.ChunkSize = 500
	}
	if options.ChunkSize < 1 || options.ChunkSize > 10000 {
		return RepriceResult{}, fmt.Errorf("invalid reprice chunk size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return RepriceResult{}, ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return RepriceResult{}, err
	}
	start := options.Range.Start
	result := RepriceResult{EffectiveStart: start, HistoryComplete: true}
	if !s.retentionCutoff.IsZero() && start.Before(s.retentionCutoff) {
		cutoff := s.retentionCutoff
		result.RetainedCutoff = &cutoff
		result.HistoryComplete = false
		start = cutoff
		if start.After(options.Range.End) {
			start = options.Range.End
		}
		result.EffectiveStart = start
	}
	digest, err := pricingCatalogDigest(s.config.PriceBook)
	if err != nil {
		return RepriceResult{}, err
	}
	key := repriceCheckpointKey(options.Range)
	if options.Resume && options.ResumeCheckpoint == "" {
		stored, _, err := s.metadata(ctx, key)
		if err != nil {
			return RepriceResult{}, err
		}
		options.ResumeCheckpoint = stored
	}
	previous, ns, id, err := parseRepriceCheckpoint(options.ResumeCheckpoint)
	if err != nil {
		return RepriceResult{}, err
	}
	if previous != "" && previous != digest {
		return RepriceResult{}, ErrRepriceCatalogChanged
	}
	statement := "SELECT event_json FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND requested_at_ns>=? AND requested_at_ns<?"
	args := []any{start.UnixNano(), options.Range.End.UnixNano()}
	if options.ResumeCheckpoint != "" {
		statement += " AND (requested_at_ns>? OR (requested_at_ns=? AND attempt_id>?))"
		args = append(args, ns, ns, id)
	}
	statement += " ORDER BY requested_at_ns,attempt_id LIMIT ?"
	args = append(args, options.ChunkSize+1)
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return RepriceResult{}, err
	}
	events := make([]model.Event, 0, options.ChunkSize+1)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return RepriceResult{}, err
		}
		var event model.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			_ = rows.Close()
			return RepriceResult{}, err
		}
		events = append(events, event)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return RepriceResult{}, err
	}
	result.Completed = len(events) <= options.ChunkSize
	if !result.Completed {
		events = events[:options.ChunkSize]
	}
	// Intermediate chunks of one run reuse the caller's count; the first and
	// final chunks recount so the reported total reflects the current range.
	result.Matched = options.Matched
	if options.Matched <= 0 || result.Completed {
		var matched uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND requested_at_ns>=? AND requested_at_ns<?", start.UnixNano(), options.Range.End.UnixNano()).Scan(&matched); err != nil {
			return RepriceResult{}, err
		}
		result.Matched = int64(matched)
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		result.Checkpoint = formatRepriceCheckpoint(digest, last.RequestedAt.UnixNano(), last.AttemptID)
	}
	if options.DryRun {
		return result, nil
	}
	for i := range events {
		priced, err := s.config.PriceBook.Price(events[i])
		if err != nil {
			return RepriceResult{}, fmt.Errorf("reprice event %d: %w", i, err)
		}
		events[i].KnownCost = priced.KnownCost
		events[i].UnpricedTokens = priced.UnpricedTokens
		events[i].PriceRuleID = priced.RuleID
		events[i].PriceSource = priced.Source
		events[i].PriceUSDPerCredit = priced.PriceUSDPerCredit
		events[i].PricingBreakdown = priced.Breakdown
	}
	if _, err := s.insertEvents(ctx, events, "", true); err != nil {
		return RepriceResult{}, err
	}
	result.Updated = int64(len(events))
	if progress != nil {
		for i := range events {
			progress(min(99, (i+1)*100/len(events)), result.Checkpoint)
		}
	}
	if result.Completed {
		if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_metadata")+" (key,value,version,is_deleted) VALUES (?,?,?,?)", key, "", s.nextVersion(), uint8(1)); err != nil {
			return RepriceResult{}, err
		}
	} else if err := s.setMetadata(ctx, key, result.Checkpoint); err != nil {
		return RepriceResult{}, err
	}
	return result, nil
}
func repriceCheckpointKey(selected model.Range) string {
	digest := sha256.Sum256([]byte(selected.Start.UTC().Format(time.RFC3339Nano) + "\x00" +
		selected.End.UTC().Format(time.RFC3339Nano) + "\x00" + selected.TimeZone))
	return "reprice_checkpoint_" + hex.EncodeToString(digest[:])
}

func pricingCatalogDigest(book aggregate.PriceBook) (string, error) {
	rules := clonePricingRules(book.Rules)
	slices.SortFunc(rules, func(left, right aggregate.PricingRule) int {
		return strings.Compare(left.ID, right.ID)
	})
	canonical, err := json.Marshal(struct {
		Rules []aggregate.PricingRule
		Rate  model.NanoUSD
	}{rules, book.EffectiveKiroRate()})
	if err != nil {
		return "", fmt.Errorf("encode reprice catalog: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func formatRepriceCheckpoint(catalogDigest string, requestedNS int64, attemptID string) string {
	return catalogDigest + ":" + strconv.FormatInt(requestedNS, 10) + ":" + attemptID
}

func parseRepriceCheckpoint(checkpoint string) (string, int64, string, error) {
	if checkpoint == "" {
		return "", 0, "", nil
	}
	parts := strings.SplitN(checkpoint, ":", 3)
	if len(parts) != 3 || !model.IsFullKeyID(parts[0]) || !model.IsCorrelationID(parts[2]) {
		return "", 0, "", fmt.Errorf("invalid reprice checkpoint")
	}
	requestedNS, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || requestedNS <= 0 {
		return "", 0, "", fmt.Errorf("invalid reprice checkpoint")
	}
	return parts[0], requestedNS, parts[2], nil
}
