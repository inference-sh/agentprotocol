package agentprotocol_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ap "github.com/inference-sh/agentprotocol"
)

// The property is what a model provider reads. A union must reach it as anyOf
// with no "type" beside it — an empty type is an invalid schema, and naming one
// branch's type would tell the model only part of what it may send.
func TestToolParameterProperty_unionHasNoTypeKey(t *testing.T) {
	t.Parallel()
	p := ap.ToolParameterProperty{
		Title:       "value",
		Description: "a label value",
		AnyOf: []ap.ToolParameterProperty{
			{Type: ap.ToolParamTypeString, Title: "value"},
			{Type: ap.ToolParamTypeNumber, Title: "value"},
		},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	_, hasType := m["type"]
	assert.False(t, hasType, "union must not carry a type: %s", raw)
	require.Len(t, m["anyOf"], 2)
	_, hasEnum := m["enum"]
	assert.False(t, hasEnum, "absent enum is omitted")
}

// A plain property is unchanged on the wire: type present, no anyOf, no enum.
func TestToolParameterProperty_scalarIsUnchanged(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(ap.ToolParameterProperty{Type: ap.ToolParamTypeString, Title: "q", Description: "d"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"string","title":"q","description":"d"}`, string(raw))
}

// Enum travels as data and survives a round trip.
func TestToolParameterProperty_enumRoundTrips(t *testing.T) {
	t.Parallel()
	in := ap.ToolParameterProperty{Type: ap.ToolParamTypeString, Title: "sort", Enum: []any{"count", "first_seen"}}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	var out ap.ToolParameterProperty
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in.Enum, out.Enum)
	assert.Equal(t, ap.ToolParamTypeString, out.Type)
}
