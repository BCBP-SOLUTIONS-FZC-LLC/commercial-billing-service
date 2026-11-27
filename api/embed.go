// Package api embeds the AsyncAPI contract, served at /asyncapi.yaml (LLD §7.6).
package api

import _ "embed"

// AsyncAPI is the raw AsyncAPI 3.0 document.
//
//go:embed asyncapi.yaml
var AsyncAPI []byte
