package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

var ErrRepriceCatalogChanged = errors.New("pricing catalog changed since the reprice checkpoint")

type RepriceOptions struct {
	Range            model.Range
	DryRun           bool
	ChunkSize        int
	Resume           bool
	ResumeCheckpoint string
}

type RepriceResult struct {
	Matched         int64
	Updated         int64
	Checkpoint      string
	Completed       bool
	EffectiveStart  time.Time
	RetainedCutoff  *time.Time
	HistoryComplete bool
}

func (s *SQLiteStore) Reprice(ctx context.Context, options RepriceOptions, progress func(int, string)) (RepriceResult, error) {
	if options.Range.Start.IsZero() || options.Range.End.IsZero() || !options.Range.Start.Before(options.Range.End) {
		return RepriceResult{}, fmt.Errorf("invalid reprice range")
	}
	if options.ChunkSize == 0 {
		options.ChunkSize = 500
	}
	if options.ChunkSize < 1 || options.ChunkSize > 10_000 {
		return RepriceResult{}, fmt.Errorf("invalid reprice chunk size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return RepriceResult{}, ErrClosed
	}
	effectiveStart := options.Range.Start
	result := RepriceResult{EffectiveStart: effectiveStart, HistoryComplete: true}
	if !s.retentionCutoff.IsZero() && effectiveStart.Before(s.retentionCutoff) {
		cutoff := s.retentionCutoff
		result.RetainedCutoff = &cutoff
		result.HistoryComplete = false
		effectiveStart = cutoff
		if effectiveStart.After(options.Range.End) {
			effectiveStart = options.Range.End
		}
		result.EffectiveStart = effectiveStart
	}
	catalogDigest, err := pricingCatalogDigest(s.config.PriceBook)
	if err != nil {
		return RepriceResult{}, err
	}
	checkpointKey := repriceCheckpointKey(options.Range)
	if options.Resume && options.ResumeCheckpoint == "" {
		var stored string
		errCheckpoint := s.db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key=?", checkpointKey).Scan(&stored)
		if errCheckpoint != nil && errCheckpoint != sql.ErrNoRows {
			return RepriceResult{}, fmt.Errorf("load reprice checkpoint: %w", errCheckpoint)
		}
		options.ResumeCheckpoint = stored
	}
	checkpointDigest, resumeNS, resumeID, err := parseRepriceCheckpoint(options.ResumeCheckpoint)
	if err != nil {
		return RepriceResult{}, err
	}
	if checkpointDigest != "" && checkpointDigest != catalogDigest {
		return RepriceResult{}, ErrRepriceCatalogChanged
	}
	var matched int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
WHERE requested_at_ns >= ? AND requested_at_ns < ?`, options.Range.Start.UnixNano(), options.Range.End.UnixNano()).Scan(&matched); err != nil {
		return RepriceResult{}, fmt.Errorf("count reprice events: %w", err)
	}
	statement := `SELECT ` + eventSelect + ` FROM events
WHERE requested_at_ns >= ? AND requested_at_ns < ?`
	arguments := []any{options.Range.Start.UnixNano(), options.Range.End.UnixNano()}
	if options.ResumeCheckpoint != "" {
		statement += ` AND (requested_at_ns > ? OR (requested_at_ns = ? AND attempt_id > ?))`
		arguments = append(arguments, resumeNS, resumeNS, resumeID)
	}
	statement += ` ORDER BY requested_at_ns,attempt_id LIMIT ?`
	arguments = append(arguments, options.ChunkSize+1)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return RepriceResult{}, fmt.Errorf("query reprice events: %w", err)
	}
	events := make([]model.Event, 0, options.ChunkSize+1)
	for rows.Next() {
		event, errScan := scanEvent(rows)
		if errScan != nil {
			_ = rows.Close()
			return RepriceResult{}, errScan
		}
		events = append(events, event)
	}
	errRows := rows.Err()
	_ = rows.Close()
	if errRows != nil {
		return RepriceResult{}, fmt.Errorf("read reprice events: %w", errRows)
	}
	hasMore := len(events) > options.ChunkSize
	if hasMore {
		events = events[:options.ChunkSize]
	}
	result.Matched, result.Completed = matched, !hasMore
	if len(events) != 0 {
		last := events[len(events)-1]
		result.Checkpoint = formatRepriceCheckpoint(catalogDigest, last.RequestedAt.UnixNano(), last.AttemptID)
	}
	if options.DryRun || len(events) == 0 {
		if !options.DryRun && result.Completed {
			if _, err := s.db.ExecContext(ctx, "DELETE FROM analytics_metadata WHERE key=?", checkpointKey); err != nil {
				return RepriceResult{}, fmt.Errorf("clear reprice checkpoint: %w", err)
			}
		}
		return result, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RepriceResult{}, fmt.Errorf("begin reprice chunk: %w", err)
	}
	for index, event := range events {
		priced, errPrice := s.price(event)
		if errPrice != nil {
			_ = tx.Rollback()
			return RepriceResult{}, fmt.Errorf("reprice event %d: %w", index, errPrice)
		}
		var knownCost any
		if priced.KnownCost != nil {
			knownCost = int64(*priced.KnownCost)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE events SET known_cost_nano=?,unpriced_tokens=?,price_rule_id=?,price_source=?
WHERE attempt_id=?`, knownCost, priced.UnpricedTokens, nullString(priced.RuleID), nullString(priced.Source), event.AttemptID); err != nil {
			_ = tx.Rollback()
			return RepriceResult{}, fmt.Errorf("update repriced event %d: %w", index, err)
		}
		result.Updated++
		if progress != nil {
			progress(min(99, int(result.Updated*100/int64(len(events)))), result.Checkpoint)
		}
	}
	if result.Completed {
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_metadata WHERE key=?", checkpointKey); err != nil {
			_ = tx.Rollback()
			return RepriceResult{}, fmt.Errorf("clear reprice checkpoint: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_metadata(key,value) VALUES (?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, checkpointKey, result.Checkpoint); err != nil {
		_ = tx.Rollback()
		return RepriceResult{}, fmt.Errorf("save reprice checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RepriceResult{}, fmt.Errorf("commit reprice chunk: %w", err)
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
	canonical, err := json.Marshal(rules)
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
