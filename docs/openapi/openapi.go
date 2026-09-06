// Package openapi carries the OpenAPI description of the tentacron API.
//
// openapi.yaml is the single source of truth for the HTTP contract. It lives
// under docs/ like the sibling services' descriptions (buem-gateway, weather)
// so the standalone viewer next to it, index.html, can render it by relative
// path — from the published site, from a checkout, or from the raw file.
// An embed directive cannot reach outside its package's directory, so this
// package embeds the file for the API package, which serves it at
// GET /openapi.yaml; a running service therefore always describes its own
// build, and a test keeps the document in lockstep with the registered
// routes.
package openapi

import _ "embed"

// Spec is the OpenAPI document, byte for byte as committed.
//
//go:embed openapi.yaml
var Spec []byte
