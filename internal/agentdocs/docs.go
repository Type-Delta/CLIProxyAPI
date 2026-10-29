// Package agentdocs embeds the public agent documentation shipped with CPA.
package agentdocs

import "embed"

// Files contains the index and its Markdown companion pages.
//
//go:embed llms.txt management.md sql.md
var Files embed.FS
