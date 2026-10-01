// Package modules embeds the built-in module specs.
package modules

import "embed"

//go:embed *.yaml
var FS embed.FS
