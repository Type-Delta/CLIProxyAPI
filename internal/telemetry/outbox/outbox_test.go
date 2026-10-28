package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

func TestRoundTripAndCompleteFiles(t *testing.T) {
	dir := t.TempDir()
	want := &record.Request{RequestID: "abc", StartedAt: time.Unix(10, 0).UTC(), Body: []byte("request"), Attempts: []record.Attempt{{Index: 1, Response: []byte("response")}}}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, directory, "unfinished.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("complete paths: %v", paths)
	}
	got, err := Read(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if err := Write(dir, &record.Request{RequestID: "../escape"}); err == nil {
		t.Fatal("accepted invalid request ID")
	}
}

func TestReadRejectsTruncatedFrameAfterCompleteJSON(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, &record.Request{RequestID: "truncated", StartedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	paths, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths[0], data[:len(data)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Read(paths[0]); err == nil {
		t.Fatal("accepted truncated zstd frame")
	}
}

func TestConcurrentCompressionReuse(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				r := &record.Request{RequestID: fmt.Sprintf("worker-%d-%d", worker, n), StartedAt: time.Unix(1, 0).UTC(), Body: []byte(fmt.Sprintf("payload-%d-%d", worker, n))}
				if err := Write(dir, r); err != nil {
					errs <- err
					return
				}
				path := filepath.Join(dir, directory, fmt.Sprintf("%d-%s.json.zst", r.StartedAt.UnixNano(), r.RequestID))
				got, err := Read(path)
				if err != nil {
					errs <- err
					return
				}
				if !reflect.DeepEqual(got, r) {
					errs <- fmt.Errorf("compression state leaked between records: got %#v want %#v", got, r)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
