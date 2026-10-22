//go:build darwin

package container

import (
	"os"
	"syscall"
	"time"
)

// dirCreatedAt returns the birth time of the dir.
func dirCreatedAt(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Birthtimespec.Unix())
	}
	return info.ModTime()
}
