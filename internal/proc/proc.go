// Package proc reads the process table for liveness checks: each process's
// arguments, executable, working directory, HOME and open files. It only
// reads metadata the kernel exposes and never a process's memory or files.
package proc

// Process is one running process, as far as it could be read.
type Process struct {
	PID  int
	Argv []string
	Exe  string
	CWD  string
	// Home is the process's HOME, or "" when its environment is unreadable.
	Home string
	// Open are the paths of the files and directories it holds open.
	Open []string
}
