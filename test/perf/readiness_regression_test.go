package perf

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type r3CircuitService struct{ cpauk.Service }

func (r3CircuitService) Capabilities() cpauk.Capabilities {
	return cpauk.Capabilities{Available: true, State: model.StateCircuitOpen}
}
func (r3CircuitService) Health() cpauk.Health {
	return cpauk.Health{State: model.StateCircuitOpen, Category: "storage_io"}
}
func TestR3PerfStartupRejectsClosedIntake(t *testing.T) {
	if err := waitForAnalyticsState(r3CircuitService{}, true, time.Second); err == nil {
		t.Fatal("startup guard accepted circuit_open because reads remain available")
	}
}
