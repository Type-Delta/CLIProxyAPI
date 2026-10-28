package config

import "testing"

func TestUnusedClickHouseConfigDoesNotRejectPlainMode(t *testing.T) {
	c := ClickHouseConfig{}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
