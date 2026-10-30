package clickhouse

import (
	ch "github.com/ClickHouse/clickhouse-go/v2"
	"testing"
)

func TestS4CompressionOptions(t *testing.T) {
	for _, tt := range []struct {
		dsn    string
		method ch.CompressionMethod
	}{
		{"clickhouse://localhost:9000", ch.CompressionLZ4},
		{"clickhouse://localhost:9000?compress=false", ch.CompressionNone},
		{"clickhouse://localhost:9000?compress=zstd", ch.CompressionZSTD},
		{"clickhouse://localhost:9000?compress_level=4", ch.CompressionLZ4},
	} {
		opts, err := connectionOptions(tt.dsn)
		if err != nil {
			t.Fatal(err)
		}
		method := ch.CompressionNone
		if opts.Compression != nil {
			method = opts.Compression.Method
		}
		if method != tt.method {
			t.Errorf("%s: got %v want %v", tt.dsn, method, tt.method)
		}
	}
}
