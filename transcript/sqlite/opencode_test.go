package sqlite

import (
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The opencode sample is one real session extracted from a live database: a
// prompt, an assistant turn with a read tool call, and its answer.
const (
	opencodeCWD = "/home/testuser/test-repo"
	opencodeID  = "ses_f3314d1b3ffegWL15JAVSbsnbM"
)

func TestOpencodeRead(t *testing.T) {
	st, err := Opencode.Open("testdata/opencode")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), opencodeCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != opencodeID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), opencodeID)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != opencodeCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var user, call, result bool
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleUser {
			user = true
		}
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse {
				call = true
			}
			if b.Kind == transcript.BlockToolResult {
				result = true
			}
		}
	}
	if !user || !call || !result {
		t.Errorf("user %v call %v result %v", user, call, result)
	}
	if countTurns(s) == 0 {
		t.Error("no turn in events")
	}
}

// The kilo sample is one ACP run captured the same way; kilo shares
// opencode's schema.
const (
	kiloCWD = "/home/testuser/test-repo"
	kiloID  = "ses_f33145df9ffeeXTyEn5hwHXfJU"
)

func TestKiloRead(t *testing.T) {
	st, err := Kilo.Open("testdata/kilo")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), kiloCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != kiloID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), kiloID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages()) == 0 {
		t.Error("no messages")
	}
}

func TestOpencodeRoundTrip(t *testing.T) { roundTrip(t, Opencode, "opencode") }
func TestKiloRoundTrip(t *testing.T)     { roundTrip(t, Kilo, "kilo") }

func TestOpencodeKiloRegistered(t *testing.T) {
	for _, agent := range []string{"opencode", "kilo"} {
		if _, ok := transcript.Registered(agent); !ok {
			t.Errorf("%s not registered", agent)
		}
	}
}
