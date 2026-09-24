package codexapp

// The protocol types are derived from the schema the installed codex binary
// publishes (`codex app-server generate-json-schema`). Regenerating needs
// codex on PATH; the version it reports is recorded in protocol_gen.go.
//go:generate go run ./internal/schemagen -out protocol_gen.go
