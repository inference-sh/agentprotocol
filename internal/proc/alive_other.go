//go:build !unix

package proc

// Alive cannot tell off Unix and says no.
func Alive(pid int) bool { return false }
