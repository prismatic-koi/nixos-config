//go:build !darwin

package container

import (
	"os"
	"time"
)

// dirCreatedAt returns the modification time of the dir. This platform
// gives no birth time in Stat_t. The modification time is never earlier
// than the birth time, so the sweep keeps a dir in each case where the
// birth time keeps it.
func dirCreatedAt(info os.FileInfo) time.Time {
	return info.ModTime()
}
