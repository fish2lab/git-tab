//go:build !darwin && !linux && !windows

package agents

import (
	"os"
	"time"
)

// born is when the file at path was last modified: this system's creation
// time is not read.
func born(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
