package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
)

// probeAnswerText is what the questions probe answers each question with,
// as an answer of its own: text nothing else in a run contains, so finding
// it in a later request to the model shows the answer arrived.
const probeAnswerText = "probe-answer-7f3c"

// probeAnswer fills in a form an agent's question became: every field for
// an answer of the person's own gets probeAnswerText, and nothing is chosen.
func probeAnswer(schema json.RawMessage) map[string]any {
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	values := map[string]any{}
	for key, p := range s.Properties {
		if p.Type == "string" && strings.HasSuffix(key, "_other") {
			values[key] = probeAnswerText
		}
	}
	return values
}

// Questions is the input requests the agent raised so far.
func (d *SessionDriver) Questions() []ap.InputForm {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ap.InputForm(nil), d.questions...)
}

// probeQuestions makes the agent ask a multiple-choice question and asks
// whether the driver raised it as a form, and whether the answer given in
// that form reached the model. The mock answers the agent with the
// registry's call to its question tool once; the session answers the form
// with probeAnswerText, which the agent then sends on to the model as the
// tool's result.
func (r *TestRunner) probeQuestions() {
	if r.harness.DriverKind() == "" {
		return
	}
	fmt.Println("[probe] questions (an agent asking the person)")
	name := r.harness.Name
	tc := r.harness.ToolCallQuestion
	if tc.Name == "" || r.server == nil {
		r.skip("questions:no-tool", fmt.Sprintf("questions: %s has no question tool in the registry", name))
		return
	}
	r.server.ClearLog()
	r.server.PrepareToolCallIn(tc.Namespace, tc.Name, r.expand(tc.Args), r.harness.ToolCallPath)
	d, ok := r.startProbeTurn("questions")
	if !ok {
		return
	}
	if !r.server.ToolCallServed() {
		d.Close()
		r.skip("questions:not-offered", fmt.Sprintf("questions: %s's turn never reached the request that calls %s", name, tc.Name))
		return
	}
	d.WaitIdle(5 * time.Second)
	qs := d.Questions()
	d.Close()

	if len(qs) == 0 {
		r.finding("questions.asked:not-raised", fmt.Sprintf("questions: %s called %s and the driver raised no input request", name, tc.Name))
		return
	}
	r.pass("questions.asked", fmt.Sprintf("questions: %s's %s was raised as a form of %d field(s)", name, tc.Name, len(probeAnswer(qs[0].Schema))))

	for _, e := range r.server.Log() {
		if strings.Contains(string(e.Body), probeAnswerText) {
			r.pass("questions.answered", fmt.Sprintf("questions: the answer reached the model (%s %s)", e.Method, e.Path))
			return
		}
	}
	r.fail("questions.answered", fmt.Sprintf("questions: %s was answered %q and no later request to the model carried it", name, probeAnswerText))
}
