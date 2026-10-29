package driver

import (
	"encoding/json"
	"fmt"
	"strings"

	ap "github.com/inference-sh/agentprotocol/model"
)

// Question is one multiple-choice question an agent asks the person, in the
// shape Claude Code's AskUserQuestion and Grok's ask_user_question share. A
// driver turns its agent's questions into these, offers them as one form
// (QuestionsInput) on an approval-required event, and reads the answer back
// (QuestionAnswers) to put into the agent's own reply. Every question also
// takes an answer of the person's own, as both agents allow.
type Question struct {
	// Header is a short label for the question; Text is the question.
	Header      string
	Text        string
	Options     []QuestionOption
	MultiSelect bool
}

// QuestionOption is one choice.
type QuestionOption struct {
	Label       string
	Description string
}

// Answer is the person's answer to one question: the labels they chose, and
// what they wrote themselves, instead of choosing or as well.
type Answer struct {
	Labels []string
	Text   string
}

// Empty reports whether nothing was answered.
func (a Answer) Empty() bool { return len(a.Labels) == 0 && a.Text == "" }

// questionKey is the form property for question i; questionOtherKey the one
// for the person's own answer to it.
func questionKey(i int) string      { return fmt.Sprintf("q%d", i+1) }
func questionOtherKey(i int) string { return fmt.Sprintf("q%d_other", i+1) }

// QuestionsInput is the form that asks qs: per question a single or multiple
// choice, and a free-text field for an answer of the person's own.
func QuestionsInput(message string, qs []Question) ap.InputRequest {
	type choice struct {
		Const       string `json:"const"`
		Title       string `json:"title"`
		Description string `json:"description,omitempty"`
	}
	// Keys sort in question order (q1, q1_other, q2, ...), which is the
	// order a form lists them in; agents ask a handful at a time.
	props := map[string]any{}
	for i, q := range qs {
		choices := make([]choice, 0, len(q.Options))
		for _, o := range q.Options {
			choices = append(choices, choice{Const: o.Label, Title: o.Label, Description: o.Description})
		}
		title := q.Header
		if title == "" {
			title = q.Text
		}
		prop := map[string]any{"title": title, "description": q.Text}
		if q.MultiSelect {
			prop["type"] = "array"
			prop["items"] = map[string]any{"anyOf": choices}
		} else {
			prop["type"] = "string"
			prop["oneOf"] = choices
		}
		props[questionKey(i)] = prop
		props[questionOtherKey(i)] = map[string]any{
			"type": "string", "title": "Other",
			"description": "An answer of your own to: " + q.Text,
		}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
	return ap.InputRequest{Message: message, Schema: schema}
}

// QuestionAnswers reads the answers to qs out of a resolution's values, one
// per question in order. A value that is not one of the question's choices
// is taken as text of the person's own.
func QuestionAnswers(qs []Question, values map[string]any) []Answer {
	out := make([]Answer, len(qs))
	for i, q := range qs {
		valid := map[string]bool{}
		for _, o := range q.Options {
			valid[o.Label] = true
		}
		var a Answer
		var other []string
		for _, v := range stringsOf(values[questionKey(i)]) {
			if valid[v] {
				a.Labels = append(a.Labels, v)
			} else {
				other = append(other, v)
			}
		}
		if t := strings.TrimSpace(stringOf(values[questionOtherKey(i)])); t != "" {
			other = append(other, t)
		}
		if !q.MultiSelect && len(a.Labels) > 1 {
			a.Labels = a.Labels[:1]
		}
		a.Text = strings.Join(other, "\n")
		out[i] = a
	}
	return out
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// stringsOf reads a single choice or a multiple one.
func stringsOf(v any) []string {
	switch v := v.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
