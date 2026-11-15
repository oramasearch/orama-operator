// SPDX-License-Identifier: MIT

// Package controlplanecontract embeds the authoritative deployment OpenAPI document.
package controlplanecontract

import _ "embed"

// OpenAPI is served byte-for-byte at /v1/openapi.json.
//
//go:embed openapi.json
var OpenAPI []byte
