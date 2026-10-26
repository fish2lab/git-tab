package agents

import (
	"os"
	"syscall"
	"time"
)

// born is when the file at path was created, or failing that last modified.
func born(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.CreationTime.Nanoseconds())
	}
	return fi.ModTime()
}
