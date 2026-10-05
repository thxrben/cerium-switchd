//go:build linux

package usbstore

import (
	"errors"

	"golang.org/x/sys/unix"
)

// kernelMounter mounts through mount(2): nothing on the stick can run or
// act as a device.
type kernelMounter struct{}

func (kernelMounter) Mount(dev, dir, fstype string, readOnly bool) error {
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOATIME)
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	data := ""
	switch fstype {
	case "vfat":
		data = "utf8,shortname=mixed,uid=0,gid=0,fmask=0177,dmask=0077"
	case "exfat":
		data = "iocharset=utf8,uid=0,gid=0,fmask=0177,dmask=0077"
	}
	return unix.Mount(dev, dir, fstype, flags, data)
}

func (kernelMounter) Unmount(dir string) error {
	err := unix.Unmount(dir, 0)
	if errors.Is(err, unix.EBUSY) {
		return unix.Unmount(dir, unix.MNT_DETACH)
	}
	if errors.Is(err, unix.EINVAL) {
		return nil // not mounted
	}
	return err
}
