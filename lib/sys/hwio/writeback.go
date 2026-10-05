package hwio

import (
	"os"
	"time"
)

// WriteBackChunk is how much of a file WriteBack writes to the disk at a
// time.
const WriteBackChunk = 4 << 20

// WriteBack writes a file that was written without syncs (a download) to
// the disk WriteBackChunk bytes at a time, each with the deadline d (0:
// FileDeadline), then syncs it. On a slow disk the other files on it are
// written in between (a single sync of hundreds of megabytes would hold
// them back for minutes), and a rename of the file afterwards has nothing
// left to write.
func WriteBack(f *os.File, d time.Duration) error {
	if d <= 0 {
		d = FileDeadline()
	}
	st, err := Do(Resource(f.Name()), "stat "+f.Name(), d, f.Stat)
	if err != nil {
		return err
	}
	for off := int64(0); off < st.Size(); off += WriteBackChunk {
		if err := DoErr(Resource(f.Name()), "sync "+f.Name(), d, func() error { return writeBackRange(f, off, WriteBackChunk) }); err != nil {
			return err
		}
	}
	return DoErr(Resource(f.Name()), "sync "+f.Name(), d, f.Sync)
}

// WriteBackFile is WriteBack on a file by name.
func WriteBackFile(name string, d time.Duration) error {
	f, err := Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteBack(f, d)
}
