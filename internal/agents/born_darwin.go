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
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Birthtimespec.Unix())
	}
	return fi.ModTime()
}
