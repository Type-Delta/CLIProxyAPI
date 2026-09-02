package cpauk

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestCapabilitiesAdvertiseCompatibleAPISchemaVersions(t *testing.T) {
	if model.APISchemaVersion != 1 {
		t.Fatalf("default API schema version = %d, want 1 for viewer compatibility", model.APISchemaVersion)
	}
	got := capabilitiesFor(StateReady, DefaultConfig(), model.QueueSnapshot{}, nil)
	if !reflect.DeepEqual(got.APISchemaVersions, []int{1, 2}) {
		t.Fatalf("API schema versions = %v, want [1 2]", got.APISchemaVersions)
	}
}
