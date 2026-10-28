package chstore

import (
	"flag"
	"os"
	"runtime"
	"strconv"
	"testing"
)

// Bound independent integration fixtures so large build hosts do not open a
// database per CPU at once. An explicit -parallel still controls the test runner.
func TestMain(m *testing.M) {
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.parallel" {
			explicit = true
		}
	})
	if !explicit {
		_ = flag.Set("test.parallel", strconv.Itoa(min(8, runtime.GOMAXPROCS(0))))
	}
	os.Exit(m.Run())
}
