package model

import "encoding/json"

// InputForm is a set of questions an agent asks the person, as a form:
// form-mode elicitation as MCP and ACP define it. Schema is a flat JSON
// object schema whose properties are the questions:
//
//   - a string, number, integer or boolean field;
//   - a single choice: a string with enum (and enumNames) or oneOf
//     ({const, title}) for the choices;
//   - several choices: an array whose items have enum or anyOf.
//
// A property's title is the question's short label and its description the
// question itself. The answer is an object with a value per property; one
// left out was not answered.
//
// Agents ask in their own shapes (Claude Code's AskUserQuestion, Grok's
// x.ai/ask_user_question, ACP's elicitation/create); a driver turns each into
// an InputForm and the answer back into the agent's shape, so a person
// answers every agent the same way.
type InputForm struct {
	Message string          `json:"message,omitempty"`
	Schema  json.RawMessage `json:"requested_schema"`
}
