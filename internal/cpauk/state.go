package cpauk

import "github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"

type State = model.AnalyticsState

const (
	StateDisabled    = model.StateDisabled
	StateStarting    = model.StateStarting
	StateReady       = model.StateReady
	StateDegraded    = model.StateDegraded
	StateCircuitOpen = model.StateCircuitOpen
	StateStopping    = model.StateStopping
)
