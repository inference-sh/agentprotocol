//go:build !linux

package proc

// Snapshot is unavailable off Linux: there is no /proc to read open files
// and working directories from without a native library.
func Snapshot(keep func(argv []string, exe string) bool) ([]Process, bool) {
	return nil, false
}
