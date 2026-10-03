//go:build !linux

package hwio

import "os"

func writeBackRange(f *os.File, off, n int64) error { return nil } // the final sync writes it
