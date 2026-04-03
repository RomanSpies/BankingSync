package web

import "embed"

// TemplateFS holds the bundled HTML templates compiled into the binary.
//
//go:embed templates
var TemplateFS embed.FS
