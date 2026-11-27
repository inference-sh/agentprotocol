package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

// Skill names the lifecycle probe installs. lifeNeighbour stands for a skill
// the user installed themselves, which removing the probe's must not touch.
const (
	lifeUser      = "lifecycle-user"
	lifeProject   = "lifecycle-project"
	lifeNeighbour = "lifecycle-neighbour"
)

// probeSkillsLifecycle installs, replaces and removes skills through the
// library belt's plugin install uses (harness.InstallSkill, ListSkills,
// RemoveSkill) and asks the agent, through what it sends the model, whether
// it saw each change:
//
//   - skills.install.user / .project: the library lists the skill it wrote,
//     and skills.listed.user / .project: the agent sends it to the model;
//   - skills.reinstall: installing over an older copy leaves one copy, the
//     new one, with no file the new version does not have;
//   - skills.remove: the library no longer lists it, the user's own skill
//     beside it is untouched (skills.remove.neighbour-kept), and
//     skills.unlisted: the agent stops sending it.
//
// Each skill's description carries a token of its own, so what the model
// was sent is the evidence, not the agent's answer.
func (r *TestRunner) probeSkillsLifecycle() {
	h := r.harness
	fmt.Println("[probe] skills (install, replace and remove through the library)")
	if h.SkillsDir == "" {
		r.skip("skills:no-skills-dir", fmt.Sprintf("skills: the registry names no skills dir for %s", h.Name))
		return
	}
	project := h.ProjectSkillsDir != ""
	// Project scope is the current directory, the way a CLI run in a
	// repository sees it.
	inWorkDir := func(f func() error) error {
		old, _ := os.Getwd()
		if err := os.Chdir(r.workDir()); err != nil {
			return err
		}
		defer os.Chdir(old)
		return f()
	}
	tok := func(kind string) string { return "SKLIFE-" + kind + "-" + randHex() }
	user1, user2, proj, neighbour := tok("USER"), tok("USER2"), tok("PROJ"), tok("NEIGHBOUR")
	skill := func(name, code string, extra map[string]string) fstest.MapFS {
		fs := fstest.MapFS{"SKILL.md": {Data: []byte(fmt.Sprintf(
			"---\nname: %s\ndescription: Look up the lifecycle code %s. Use when asked for the lifecycle code.\n---\n\nThe lifecycle code is %s.\n", name, code, code))}}
		for p, body := range extra {
			fs[p] = &fstest.MapFile{Data: []byte(body)}
		}
		return fs
	}
	defer func() {
		harness.RemoveSkill(h.Name, harness.ScopeUser, lifeUser)
		harness.RemoveSkill(h.Name, harness.ScopeUser, lifeNeighbour)
		if project {
			inWorkDir(func() error { return harness.RemoveSkill(h.Name, harness.ScopeProject, lifeProject) })
		}
	}()

	if err := harness.InstallSkill(h.Name, harness.ScopeUser, skill(lifeNeighbour, neighbour, nil), lifeNeighbour); err != nil {
		r.fail("skills.install.user", "skills: "+err.Error())
		return
	}
	if err := harness.InstallSkill(h.Name, harness.ScopeUser, skill(lifeUser, user1, map[string]string{"notes/old.md": "only in the first version"}), lifeUser); err != nil {
		r.fail("skills.install.user", "skills: "+err.Error())
		return
	}
	r.reportListed("skills.install.user", harness.ScopeUser, lifeUser, true, nil)
	if project {
		err := inWorkDir(func() error {
			return harness.InstallSkill(h.Name, harness.ScopeProject, skill(lifeProject, proj, nil), lifeProject)
		})
		if err != nil {
			r.fail("skills.install.project", "skills: "+err.Error())
			project = false
		} else {
			r.reportListed("skills.install.project", harness.ScopeProject, lifeProject, true, inWorkDir)
		}
	}

	sent, ok := r.skillsTurn("skills.listed")
	if !ok {
		return
	}
	userSeen := entriesContain(sent, user1)
	r.reportSeen("skills.listed.user", userSeen, true, lifeUser)
	if project {
		r.reportSeen("skills.listed.project", entriesContain(sent, proj), true, lifeProject)
	}

	// Replace: a new description and no notes/ directory.
	if err := harness.InstallSkill(h.Name, harness.ScopeUser, skill(lifeUser, user2, nil), lifeUser); err != nil {
		r.fail("skills.reinstall", "skills: "+err.Error())
	} else {
		dir := filepath.Join(harness.SkillsPath(h.Name, harness.ScopeUser), lifeUser)
		body, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		_, staleErr := os.Stat(filepath.Join(dir, "notes", "old.md"))
		names, _ := harness.ListSkills(h.Name, harness.ScopeUser)
		copies := 0
		for _, n := range names {
			if strings.HasPrefix(n, lifeUser) {
				copies++
			}
		}
		switch {
		case !strings.Contains(string(body), user2):
			r.fail("skills.reinstall", "skills: installing over the skill left the old SKILL.md")
		case staleErr == nil:
			r.fail("skills.reinstall", "skills: installing over the skill kept a file the new version does not have (notes/old.md)")
		case copies != 1:
			r.fail("skills.reinstall", fmt.Sprintf("skills: installing over the skill left %d copies: %v", copies, names))
		default:
			r.pass("skills.reinstall", "skills: installing over the skill left one copy, the new one, with nothing from the old")
		}
	}

	if err := harness.RemoveSkill(h.Name, harness.ScopeUser, lifeUser); err != nil {
		r.fail("skills.remove", "skills: "+err.Error())
		return
	}
	if project {
		inWorkDir(func() error { return harness.RemoveSkill(h.Name, harness.ScopeProject, lifeProject) })
	}
	r.reportListed("skills.remove", harness.ScopeUser, lifeUser, false, nil)
	if _, err := os.Stat(filepath.Join(harness.SkillsPath(h.Name, harness.ScopeUser), lifeNeighbour, "SKILL.md")); err == nil {
		r.pass("skills.remove.neighbour-kept", "skills: removing the skill left the user's other skill in place")
	} else {
		r.fail("skills.remove.neighbour-kept", "skills: removing the skill took the user's other skill with it")
	}

	sent, ok = r.skillsTurn("skills.unlisted")
	if !ok {
		return
	}
	gone := !entriesContain(sent, user2) && !entriesContain(sent, user1) && (!project || !entriesContain(sent, proj))
	switch {
	case !userSeen:
		// The agent never sent the skill, so its absence now proves nothing.
		r.skip("skills.unlisted:never-listed", "skills: the agent did not send the skill while it was installed, so its removal cannot be seen")
	case gone:
		r.pass("skills.unlisted", fmt.Sprintf("skills: after removal the agent no longer sends the skill (the neighbour: sent %t)", entriesContain(sent, neighbour)))
	default:
		r.finding("skills.unlisted:still-sent", "skills: the agent still sends a removed skill to the model")
	}
}

// reportListed checks the library's own listing at a scope.
func (r *TestRunner) reportListed(label string, scope harness.InstallScope, skill string, want bool, in func(func() error) error) {
	var names []string
	list := func() error {
		var err error
		names, err = harness.ListSkills(r.harness.Name, scope)
		return err
	}
	if in != nil {
		in(list)
	} else {
		list()
	}
	has := containsName(names, skill)
	switch {
	case has == want && want:
		r.pass(label, fmt.Sprintf("skills: ListSkills returns %s after it was installed", skill))
	case has == want:
		r.pass(label, fmt.Sprintf("skills: ListSkills no longer returns %s", skill))
	case want:
		r.fail(label, fmt.Sprintf("skills: ListSkills does not return %s after it was installed: %v", skill, names))
	default:
		r.fail(label, fmt.Sprintf("skills: ListSkills still returns %s after it was removed", skill))
	}
}

// reportSeen reports whether the agent sent a skill to the model.
func (r *TestRunner) reportSeen(label string, seen, want bool, skill string) {
	if seen == want {
		r.pass(label, fmt.Sprintf("skills: the agent sent %s's description to the model", skill))
		return
	}
	r.finding(label+":not-sent", fmt.Sprintf("skills: %s was installed and the agent did not send it to the model", skill))
}

// skillsTurn runs one turn in a fresh session and returns what it sent.
func (r *TestRunner) skillsTurn(label string) ([]server.LogEntry, bool) {
	if r.harness.DriverKind() == "" || r.server == nil {
		r.skip(label+":no-session", "skills: no session driver to ask")
		return nil, false
	}
	r.resetPhase(ModeACP)
	r.server.ClearLog()
	d, ok := r.startProbeTurn(label)
	if !ok {
		return nil, false
	}
	d.Close()
	return r.entries(), true
}

func containsName(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
