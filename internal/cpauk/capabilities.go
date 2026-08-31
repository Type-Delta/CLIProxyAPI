package cpauk

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type Capabilities = model.Capabilities

func capabilitiesFor(state State, cfg Config, queue model.QueueSnapshot, lastWrite *time.Time) Capabilities {
	available := state == StateReady || state == StateDegraded
	return Capabilities{
		APISchemaVersions:   []int{model.APISchemaVersion},
		EventSchemaVersion:  model.EventSchemaVersion,
		Supported:           true,
		Enabled:             cfg.Enabled,
		Available:           available,
		Degraded:            state == StateDegraded || state == StateCircuitOpen,
		State:               state,
		StorageDriver:       "sqlite",
		StorageScope:        "instance",
		KeyIDAlgorithm:      model.KeyIDAlgorithm,
		StructuredKeys:      true,
		SharedEnforcement:   false,
		ManagementQueryV1:   true,
		ViewerV1:            true,
		Queue:               queue,
		LastSuccessfulWrite: cloneTime(lastWrite),
	}
}
