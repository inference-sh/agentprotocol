package agentprotocol

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ToolCallType represents the type field on a tool call (wire format).
type ToolCallType string

// ToolParamType represents a JSON Schema parameter type for tool definitions.
type ToolParamType string

// Tool call types
const (
	ToolTypeFunction ToolCallType = "function"
)

// Tool parameter types
const (
	ToolParamTypeObject  ToolParamType = "object"
	ToolParamTypeString  ToolParamType = "string"
	ToolParamTypeInteger ToolParamType = "integer"
	ToolParamTypeNumber  ToolParamType = "number"
	ToolParamTypeBoolean ToolParamType = "boolean"
	ToolParamTypeArray   ToolParamType = "array"
	ToolParamTypeNull    ToolParamType = "null"
)

// ToolCall represents a tool call from an LLM response (wire format)
// This is a transport object for parsing LLM responses, not a database model
type ToolCall struct {
	ID       string           `json:"id"`
	Type     ToolCallType     `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction contains the function name and arguments from an LLM tool call
type ToolCallFunction struct {
	Name      string           `json:"name"`
	Arguments StringEncodedMap `json:"arguments"`
}

// UnmarshalJSON implements custom unmarshaling for ToolCallFunction to handle empty string arguments
func (t *ToolCallFunction) UnmarshalJSON(data []byte) error {
	// Define an auxiliary struct to avoid recursive UnmarshalJSON calls
	type Aux struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var aux Aux
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Set the name
	t.Name = aux.Name

	// Handle arguments based on what we received
	argStr := string(aux.Arguments)
	if argStr == "" || argStr == `""` || argStr == `null` || argStr == `"{}"` {
		// Empty string, null, or string-encoded empty object becomes empty map
		t.Arguments = make(StringEncodedMap)
	} else if strings.HasPrefix(argStr, `"`) && strings.HasSuffix(argStr, `"`) {
		// Handle string-encoded JSON by unescaping and parsing
		unquoted, err := strconv.Unquote(argStr)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(unquoted), &t.Arguments); err != nil {
			return err
		}
	} else {
		// Otherwise unmarshal normally
		if err := json.Unmarshal(aux.Arguments, &t.Arguments); err != nil {
			return err
		}
	}

	return nil
}

// LLMUsage contains token usage and performance metrics from an LLM response
type LLMUsage struct {
	StopReason       string  `json:"stop_reason"`
	TimeToFirstToken float64 `json:"time_to_first_token"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	ReasoningTime    float64 `json:"reasoning_time"`
}

// FileRef is a lightweight reference to a file with essential metadata.
// Used in chat inputs/context instead of full File objects.
type FileRef struct {
	ID          string `json:"id,omitempty"`
	URI         string `json:"uri"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size,omitempty"`
}

// Tool represents a tool definition for LLM function calling
type Tool struct {
	Type     ToolCallType `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters,omitempty"`
	Required    *[]string      `json:"required,omitempty"`
}

type ToolParameters struct {
	Type       ToolParamType           `json:"type"`
	Title      string                  `json:"title"`
	Properties ToolParameterProperties `json:"properties,omitempty"`
	Required   *[]string               `json:"required,omitempty"`
}

type ToolParameterProperties map[string]ToolParameterProperty

type ToolParameterProperty struct {
	// Type is the JSON Schema type of the value. Empty when AnyOf is set: the
	// value then has one of several shapes, and naming a single type would be
	// telling the model something untrue about what it may send.
	Type ToolParamType `json:"type,omitempty"`
	// AnyOf lists the shapes a value may take, each described as a property of
	// its own. Anthropic and OpenAI both accept anyOf in tool schemas. Before
	// this field existed the converter picked one branch and dropped the rest,
	// so an app that accepted "401" or 401 could only tell the model about one.
	AnyOf []ToolParameterProperty `json:"anyOf,omitempty"`
	// Enum is the closed set of values a scalar may take, kept as data so a
	// consumer choosing arguments does not have to parse it back out of the
	// description.
	Enum        []any                    `json:"enum,omitempty"`
	Title       string                   `json:"title"`
	Description string                   `json:"description"`
	Properties  *ToolParameterProperties `json:"properties,omitempty"`
	Items       *ToolParameterProperty   `json:"items,omitempty"`
	Required    *[]string                `json:"required,omitempty"`
}

// NewToolCall creates a ToolCall (wire format) with the given name and params
func NewToolCall(name string, params map[string]any) ToolCall {
	// Convert params to StringEncodedMap
	paramsJSON, _ := json.Marshal(params)
	var arguments StringEncodedMap
	_ = json.Unmarshal(paramsJSON, &arguments)

	return ToolCall{
		Type: ToolTypeFunction,
		Function: ToolCallFunction{
			Name:      name,
			Arguments: arguments,
		},
	}
}

func NewTool(name string, description string, properties ToolParameterProperties, required []string) Tool {
	tool := Tool{
		Type: ToolTypeFunction,
		Function: ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  NewToolParameters(properties, required),
		},
	}

	// Patch the tool function parameters with the required fields
	if len(required) > 0 {
		tool.Function.Required = &required
	}

	return tool
}

// NewToolParameters creates a parameter schema
func NewToolParameters(properties ToolParameterProperties, required []string) ToolParameters {
	toolParameters := ToolParameters{
		Type:       ToolParamTypeObject,
		Title:      "params",
		Properties: properties,
	}

	if len(required) > 0 {
		toolParameters.Required = &required
	}

	return toolParameters
}

// StringParam creates a string property
func StringParam(title, description string) ToolParameterProperty {
	return ToolParameterProperty{
		Type:        ToolParamTypeString,
		Title:       title,
		Description: description,
	}
}

// IntegerParam creates an integer property
func IntegerParam(title, description string) ToolParameterProperty {
	return ToolParameterProperty{
		Type:        ToolParamTypeInteger,
		Title:       title,
		Description: description,
	}
}

// ObjectParam creates an object property
func ObjectParam(title, description string) ToolParameterProperty {
	return ToolParameterProperty{
		Type:        ToolParamTypeObject,
		Title:       title,
		Description: description,
	}
}

// ArrayParam creates an array property
func ArrayParam(title, description string) ToolParameterProperty {
	return ToolParameterProperty{
		Type:        ToolParamTypeArray,
		Title:       title,
		Description: description,
	}
}
