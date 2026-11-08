package notifications

import (
	"bytes"
	_ "embed"
)

//go:embed contracts/notifications.openapi.json
var openAPI []byte

// OpenAPI returns the exact provider contract for consumer compatibility checks.
func OpenAPI() []byte { return bytes.Clone(openAPI) }
