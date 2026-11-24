package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	log "github.com/sirupsen/logrus"
)

// HeaderBlock is one retained HTTP exchange. Upstream blocks cover all dispatches
// for the proxy request, since telemetry dispatches and usage attempts are not one-to-one.
type HeaderBlock struct {
	AttemptIndex int                 `json:"attempt_index,omitempty"`
	StartedAt    time.Time           `json:"started_at"`
	Method       string              `json:"method"`
	URL          string              `json:"url"`
	Headers      map[string][]string `json:"headers"`
}

type RequestHeaderDetail struct {
	Blocks []HeaderBlock `json:"blocks"`
}

// RequestHeaders reads only the requested headers, without loading bodies or transcripts.
func RequestHeaders(ctx context.Context, db *clickhouse.DB, proxyID, hop string) (RequestHeaderDetail, error) {
	result := RequestHeaderDetail{Blocks: []HeaderBlock{}}
	column := "request_headers"
	upstream := false
	switch hop {
	case "client_to_cpa":
	case "cpa_to_client":
		column = "response_headers"
	case "cpa_to_provider":
		upstream = true
	case "provider_to_cpa":
		upstream, column = true, "response_headers"
	default:
		return result, fmt.Errorf("invalid header hop")
	}
	var id, transport string
	var block HeaderBlock
	var raw string
	query := "SELECT request_id,transport,started_at,method,url," + column + " FROM " + table(db, "requests") + " FINAL WHERE proxy_request_id=? ORDER BY started_at DESC LIMIT 1 SETTINGS use_skip_indexes_if_final=1"
	err := db.Conn().QueryRow(ctx, query, proxyID).Scan(&id, &transport, &block.StartedAt, &block.Method, &block.URL, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if transport == record.TransportWebsocket {
		return result, nil
	}
	if !upstream {
		block.Headers = headers(raw)
		if block.Headers != nil {
			result.Blocks = append(result.Blocks, block)
		}
		return result, nil
	}
	rows, err := db.Conn().Query(ctx, "SELECT attempt_index,started_at,method,url,"+column+" FROM "+table(db, "attempts")+" FINAL WHERE request_id=? AND transport != ? ORDER BY attempt_index", id, record.TransportWebsocket)
	if err != nil {
		return result, err
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Warn("close telemetry header rows")
		}
	}()
	for rows.Next() {
		var item HeaderBlock
		var index uint32
		if err = rows.Scan(&index, &item.StartedAt, &item.Method, &item.URL, &raw); err != nil {
			return result, err
		}
		item.AttemptIndex, item.Headers = int(index), headers(raw)
		if item.Headers != nil {
			result.Blocks = append(result.Blocks, item)
		}
	}
	return result, rows.Err()
}
