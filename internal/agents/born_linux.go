package agents

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// born is when the file at path was created, or failing that last modified.
func born(path string) time.Time {
	var st unix.Statx_t
	if unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st) == nil && st.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
