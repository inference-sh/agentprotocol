//go:build linux

package proc

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// Snapshot reads the processes keep accepts, with their open files, from
// /proc. It reports false when /proc cannot be read. A process that exits
// or cannot be inspected mid-read is skipped.
func Snapshot(keep func(argv []string, exe string) bool) ([]Process, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		var argv []string
		for _, a := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
			argv = append(argv, string(a))
		}
		exe, _ := os.Readlink(filepath.Join(dir, "exe"))
		if !keep(argv, exe) {
			continue
		}
		p := Process{PID: pid, Argv: argv, Exe: exe}
		p.CWD, _ = os.Readlink(filepath.Join(dir, "cwd"))
		if env, err := os.ReadFile(filepath.Join(dir, "environ")); err == nil {
			for _, kv := range bytes.Split(env, []byte{0}) {
				if v, ok := bytes.CutPrefix(kv, []byte("HOME=")); ok {
					p.Home = string(v)
				}
			}
		}
		fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil && filepath.IsAbs(t) {
				p.Open = append(p.Open, t)
			}
		}
		out = append(out, p)
	}
	return out, true
}
