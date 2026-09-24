package all

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/harness"
	"github.com/inference-sh/agentprotocol/internal/proc"
	"github.com/inference-sh/agentprotocol/transcript"
)

// RecentWindow is how recently a session must have been written for Live
// to call it active on the write alone.
const RecentWindow = 2 * time.Minute

// Probe answers whether sessions are in use, from one read of the process
// table, so a caller checking many sessions reads it once. It only reads
// metadata: process arguments, working directories, HOME, open file paths,
// and the agents' in-use markers. It never opens a credential file.
type Probe struct {
	home  string
	procs []proc.Process
	ok    bool
	now   time.Time
}

// NewProbe reads the agent processes running as home's user now. Off Linux
// the process table is unavailable, and answers fall back to in-use markers
// and recent writes, labelled as such.
func NewProbe(home string) *Probe {
	procs, ok := proc.Snapshot(func(argv []string, exe string) bool {
		for _, h := range harness.All {
			if h.Runs(argv, exe) {
				return true
			}
		}
		return false
	})
	return &Probe{home: filepath.Clean(home), procs: procs, ok: ok, now: time.Now()}
}

// Live reports whether one session is in use, from a fresh probe.
func Live(home string, s Session) transcript.Liveness {
	return NewProbe(home).Live(s)
}

// Live reports whether a session is in use, and what that rests on. The
// strongest evidence available decides:
//
//  1. an in-use marker the agent wrote for the session names a live process;
//  2. an agent process holds the session's own file or directory open;
//  3. no process of the agent runs at all (idle);
//  4. an agent process runs in the session's directory (heuristic: it may be
//     serving another session there);
//  5. the session was written within RecentWindow (heuristic);
//  6. the agent runs only elsewhere (heuristic idle).
//
// Without a process table only 1 and 5 apply; otherwise the answer is
// unknown. A caller should treat a heuristic or unknown answer as reason to
// warn before continuing the session, not as proof either way.
func (p *Probe) Live(s Session) transcript.Liveness {
	for _, pid := range s.Holders {
		if proc.Alive(pid) {
			return transcript.Liveness{State: transcript.LiveActive, Evidence: transcript.EvidenceLockFile, PID: pid,
				Detail: "the agent's in-use marker for this session names a running process"}
		}
	}
	recent := !s.Updated.IsZero() && p.now.Sub(s.Updated) < RecentWindow
	if !p.ok {
		if recent {
			return transcript.Liveness{State: transcript.LiveActive, Evidence: transcript.EvidenceRecentWrite, Heuristic: true,
				Detail: "written in the last " + RecentWindow.String() + "; no process table on this platform"}
		}
		return transcript.Liveness{State: transcript.LiveUnknown, Evidence: transcript.EvidenceNone,
			Detail: "no process table on this platform and no in-use marker"}
	}
	h, known := harness.All[s.Agent]
	var mine []proc.Process
	for _, pr := range p.procs {
		if known && h.Runs(pr.Argv, pr.Exe) && (pr.Home == "" || filepath.Clean(pr.Home) == p.home) {
			mine = append(mine, pr)
		}
	}
	if s.Root != "" {
		for _, pr := range mine {
			for _, f := range pr.Open {
				if within(f, s.Root) {
					return transcript.Liveness{State: transcript.LiveActive, Evidence: transcript.EvidenceOpenFile, PID: pr.PID,
						Detail: "the agent holds " + f + " open"}
				}
			}
		}
	}
	if len(mine) == 0 {
		return transcript.Liveness{State: transcript.LiveIdle, Evidence: transcript.EvidenceNoProcess,
			Detail: "no process of " + s.Agent + " is running"}
	}
	if s.CWD != "" {
		for _, pr := range mine {
			if pr.CWD != "" && within(pr.CWD, s.CWD) {
				return transcript.Liveness{State: transcript.LiveActive, Evidence: transcript.EvidenceProcessInCwd, Heuristic: true, PID: pr.PID,
					Detail: s.Agent + " is running in the session's directory; it may be serving another session there"}
			}
		}
	}
	if recent {
		return transcript.Liveness{State: transcript.LiveActive, Evidence: transcript.EvidenceRecentWrite, Heuristic: true,
			Detail: "written in the last " + RecentWindow.String()}
	}
	if s.CWD == "" {
		return transcript.Liveness{State: transcript.LiveUnknown, Evidence: transcript.EvidenceNone,
			Detail: s.Agent + " is running and the session's directory is unknown"}
	}
	return transcript.Liveness{State: transcript.LiveIdle, Evidence: transcript.EvidenceNoProcessInCwd, Heuristic: true,
		Detail: s.Agent + " is running, but not in the session's directory"}
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
