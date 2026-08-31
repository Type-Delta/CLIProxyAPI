package cpauk

import (
	"errors"
	"fmt"
)

var (
	ErrDisabled    = errors.New("analytics disabled")
	ErrUnavailable = errors.New("analytics unavailable")
	ErrInternal    = errors.New("analytics internal failure")
	ErrClosed      = errors.New("analytics closed")
	ErrMaintenance = errors.New("analytics maintenance in progress")
)

type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	if e == nil {
		return "invalid analytics configuration"
	}
	return fmt.Sprintf("invalid analytics configuration field %s: %s", e.Field, e.Reason)
}

type UnavailableError struct {
	Category string
}

func (e *UnavailableError) Error() string { return ErrUnavailable.Error() }
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// StoreErrorClassification lets the store mark permanent failures without
// coupling the collector to a concrete database implementation.
type StoreErrorClassification interface {
	error
	Permanent() bool
	Category() string
}
