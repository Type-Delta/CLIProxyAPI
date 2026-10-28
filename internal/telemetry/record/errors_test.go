package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ClickHouse/ch-go/proto"
	ch "github.com/ClickHouse/clickhouse-go/v2"
)

func TestDataErrorClassification(t *testing.T) {
	for _, code := range []proto.Error{proto.ErrTypeMismatch, proto.ErrBadArguments, proto.ErrIncorrectData, proto.ErrCannotParseText, proto.ErrCannotParseDate, proto.ErrCannotParseUUID, proto.ErrCannotInsertNullInOrdinaryColumn} {
		if !DataError(fmt.Errorf("write: %w", &ch.Exception{Code: int32(code)})) {
			t.Errorf("data code %s was retryable", code)
		}
	}
	for _, code := range []proto.Error{proto.ErrTooManyParts, proto.ErrMemoryLimitExceeded, proto.ErrReadonly, proto.ErrTableIsReadOnly, proto.ErrTimeoutExceeded, proto.ErrSocketTimeout, proto.ErrNetworkError, proto.ErrUnknownException, proto.ErrUnknownTable, proto.Error(999999)} {
		if DataError(fmt.Errorf("write: %w", &ch.Exception{Code: int32(code)})) {
			t.Errorf("infrastructure/unknown code %s was permanent", code)
		}
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded, ErrIngestPaused, errors.New("unknown native driver error")} {
		if DataError(err) {
			t.Errorf("unclassified error was permanent: %v", err)
		}
	}
	if !DataError(fmt.Errorf("decode: %w", ErrInvalidRecord)) {
		t.Fatal("local invalid record was retryable")
	}
}
