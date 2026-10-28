package chstore

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/importer"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/maintenance"
)

var _ maintenance.Store = (*Store)(nil)
var _ importer.Destination = (*Store)(nil)
