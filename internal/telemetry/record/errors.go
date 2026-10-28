package record

import (
	"context"
	"errors"

	"github.com/ClickHouse/ch-go/proto"
	ch "github.com/ClickHouse/clickhouse-go/v2"
)

// ErrInvalidRecord marks a locally validated deterministic capture/decode error.
var ErrInvalidRecord = errors.New("invalid telemetry record")

// DataError reports only deterministic data rejection. Availability, capacity,
// permissions, canceled operations and unrecognized errors always remain retryable.
// BAD_ARGUMENTS (36) is ambiguous: ClickHouse also uses it for some non-data
// conditions. Keep it here for invalid payloads; ingestion requires three
// consecutive data-error attempts before skipping/quarantining, giving transient
// conditions time to recover. A persistent non-data BAD_ARGUMENTS can still be
// misclassified, so callers log each attempt and the eventual permanent action.
func DataError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrIngestPaused) {
		return false
	}
	if errors.Is(err, ErrInvalidRecord) {
		return true
	}
	var exception *ch.Exception
	if !errors.As(err, &exception) {
		return false
	}
	switch proto.Error(exception.Code) {
	case proto.ErrCannotParseText, proto.ErrCannotParseEscapeSequence, proto.ErrCannotParseQuotedString,
		proto.ErrCannotParseInputAssertionFailed, proto.ErrBadArguments, proto.ErrCannotParseDate,
		proto.ErrCannotParseDatetime, proto.ErrIllegalTypeOfArgument, proto.ErrTypeMismatch,
		proto.ErrCannotConvertType, proto.ErrCannotParseNumber, proto.ErrIncorrectData,
		proto.ErrCannotInsertValueOfDifferentSizeIntoTuple, proto.ErrCannotReadArrayFromText,
		proto.ErrValueIsOutOfRangeOfDataType, proto.ErrCannotInsertNullInOrdinaryColumn,
		proto.ErrBadCast, proto.ErrCannotParseUUID, proto.ErrDecimalOverflow, proto.ErrCannotParseBool:
		return true
	}
	return false
}
