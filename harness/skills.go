package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skillFile is the file that makes a directory a skill. Agents match it
// exactly; skill.md is not one.
const skillFile = "SKILL.md"

// SkillsPath is the directory an agent loads skills from at a scope: the
// user's (SkillsDir under $HOME, moved by ConfigDirEnv) or the project's
// (ProjectSkillsDir under the working directory). Empty when the agent has
// none at that scope.
func SkillsPath(name string, scope InstallScope) string {
	h, ok := All[name]
	if !ok {
		return ""
	}
	if scope == ScopeProject {
		if h.ProjectSkillsDir == "" {
			return ""
		}
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, h.ProjectSkillsDir)
	}
	if h.SkillsDir == "" {
		return ""
	}
	return homePath(h, h.SkillsDir)
}

// homePath resolves a path under $HOME that ConfigDirEnv may move: the env
// var stands for the path's first segment when that segment is the agent's
// config dir, and XDG_CONFIG_HOME stands for ".config". codex's skills are
// under .agents, which CODEX_HOME does not move.
func homePath(h Harness, rel string) string {
	rel = filepath.ToSlash(rel)
	first, rest, _ := strings.Cut(rel, "/")
	if env := h.ConfigDirEnv; env != "" {
		if dir := os.Getenv(env); dir != "" {
			own, _, _ := strings.Cut(filepath.ToSlash(h.HookConfigDir), "/")
			if env == "XDG_CONFIG_HOME" && first == ".config" || env != "XDG_CONFIG_HOME" && first == own {
				return filepath.Join(dir, filepath.FromSlash(rest))
			}
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, filepath.FromSlash(rel))
}

func validSkillName(skillName string) error {
	if skillName == "" || skillName == "." || skillName == ".." || strings.ContainsAny(skillName, `/\`) || strings.Contains(skillName, "..") {
		return fmt.Errorf("invalid skill name %q", skillName)
	}
	return nil
}

// InstallSkill copies a skill into an agent's skills dir as <skillName>,
// replacing an earlier copy. skill is the skill's own directory: SKILL.md at
// its root.
//
// Skills are copied, never linked. Agents have not all followed links into
// their skills dir: Cursor did not load symlinked skills, Antigravity ignored
// symlinked skill directories, and OpenClaw refuses a link that resolves
// outside its own root (vercel-labs/skills #105, #633, #691). Links inside
// the skill are skipped for the same reason.
func InstallSkill(name string, scope InstallScope, skill fs.FS, skillName string) error {
	if err := validSkillName(skillName); err != nil {
		return err
	}
	root := SkillsPath(name, scope)
	if root == "" {
		return fmt.Errorf("%s has no skills dir at this scope", name)
	}
	if _, err := fs.Stat(skill, skillFile); err != nil {
		return fmt.Errorf("skill %s has no %s", skillName, skillFile)
	}
	dest := filepath.Join(root, skillName)
	tmp := dest + ".installing"
	os.RemoveAll(tmp)
	err := fs.WalkDir(skill, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(tmp, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0755)
		case !d.Type().IsRegular():
			return nil
		}
		data, err := fs.ReadFile(skill, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("skill %s: %w", skillName, err)
	}
	if err := os.RemoveAll(dest); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// RemoveSkill removes <skillName> from an agent's skills dir. A link there is
// removed as a link; what it points to is left alone. A missing skill is not
// an error.
func RemoveSkill(name string, scope InstallScope, skillName string) error {
	if err := validSkillName(skillName); err != nil {
		return err
	}
	root := SkillsPath(name, scope)
	if root == "" {
		return nil
	}
	dest := filepath.Join(root, skillName)
	fi, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(dest)
	}
	return os.RemoveAll(dest)
}

// ListSkills lists the skills in an agent's skills dir: each directory, or
// link to one, holding SKILL.md.
func ListSkills(name string, scope InstallScope) ([]string, error) {
	root := SkillsPath(name, scope)
	if root == "" {
		return nil, fmt.Errorf("%s has no skills dir at this scope", name)
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(root, e.Name(), skillFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
