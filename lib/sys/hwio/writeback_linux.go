//go:build linux

package hwio

import (
	"os"

	"golang.org/x/sys/unix"
)

func writeBackRange(f *os.File, off, n int64) error {
	return unix.SyncFileRange(int(f.Fd()), off, n,
		unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
}
