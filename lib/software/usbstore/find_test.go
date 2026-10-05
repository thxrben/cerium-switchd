package usbstore

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// fakeSys builds a sysfs tree: disks with their partitions (name ->
// GPT name), on a USB bus or not.
type fakeSys struct {
	t    *testing.T
	root string
}

func (f fakeSys) write(p, s string) {
	f.t.Helper()
	p = filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f fakeSys) link(target, name string) {
	f.t.Helper()
	name = filepath.Join(f.root, name)
	os.MkdirAll(filepath.Dir(name), 0o755)
	if err := os.Symlink(filepath.Join(f.root, target), name); err != nil {
		f.t.Fatal(err)
	}
}

// disk adds /sys/block/<name> with major:minor 8:<minor> for the disk and
// following minors for the partitions; it returns the disk's real path.
func (f fakeSys) disk(name string, usb bool, minor int, parts ...[2]string) string {
	scsi := "devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0"
	if usb {
		scsi = "devices/pci0000:00/0000:00:14.0/usb2/2-" + name[2:] + "/2-" + name[2:] + ":1.0/host6/target6:0:0/6:0:0:" + name[2:]
	}
	dev := scsi + "/block/" + name
	f.write(dev+"/size", "2048000\n")
	f.write(dev+"/removable", "0\n")
	f.write(scsi+"/vendor", "Kingston\n")
	f.write(scsi+"/model", "DataTraveler 3.0\n")
	f.link(scsi, dev+"/device")
	f.link(dev, "block/"+name)
	f.link(dev, "dev/block/8:"+itoa(minor))
	for i, p := range parts {
		pd := dev + "/" + p[0]
		f.write(pd+"/partition", itoa(i+1)+"\n")
		f.write(pd+"/uevent", "MAJOR=8\nDEVNAME="+p[0]+"\nPARTNAME="+p[1]+"\n")
		f.link(pd, "dev/block/8:"+itoa(minor+i+1))
	}
	return dev
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestSticks(t *testing.T) {
	f := fakeSys{t, t.TempDir()}
	// sda: the cerOS system disk, itself a USB stick (physw4).
	f.disk("sda", true, 0, [2]string{"sda1", "ceros-esp"}, [2]string{"sda2", "ceros-a"}, [2]string{"sda3", "ceros-b"})
	// sdb: a Debian system on USB below dm-verity (mounted, no cerOS names).
	sdb := f.disk("sdb", true, 16, [2]string{"sdb1", "root"})
	f.write("devices/virtual/block/dm-0/size", "100\n")
	f.link(sdb+"/sdb1", "devices/virtual/block/dm-0/slaves/sdb1")
	f.link("devices/virtual/block/dm-0", "dev/block/254:0")
	// sdc: an internal SATA disk (no USB): never a stick.
	f.disk("sdc", false, 32)
	// sdd: the data stick, two partitions (FAT first).
	f.disk("sdd", true, 48, [2]string{"sdd1", "data"}, [2]string{"sdd2", ""})
	// sde: a card reader without a card.
	sde := f.disk("sde", true, 64)
	f.write(sde+"/size", "0\n")
	mi := filepath.Join(f.root, "mountinfo")
	os.WriteFile(mi, []byte(
		"22 1 254:0 / / ro,relatime - squashfs /dev/mapper/root ro\n"+
			"23 22 0:21 / /proc rw - proc proc rw\n"+
			"24 22 8:3 / /config rw - ext4 /dev/sda3 rw\n"), 0o644)
	fd := Finder{SysRoot: f.root, MountInfo: mi}
	ss, err := fd.Sticks()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range ss {
		names = append(names, s.Disk)
	}
	if !slices.Equal(names, []string{"sdd"}) {
		t.Fatalf("sticks %v (want only sdd: sda has cerOS partitions, sdb is mounted through dm-0, sdc is not USB, sde has no medium)", names)
	}
	s := ss[0]
	if !slices.Equal(s.Devices, []string{"/dev/sdd1", "/dev/sdd2"}) || s.Size != 2048000*512 || s.Vendor != "Kingston" {
		t.Fatalf("stick %+v", s)
	}
}

func TestNoStick(t *testing.T) {
	f := fakeSys{t, t.TempDir()}
	f.disk("sda", true, 0, [2]string{"sda1", "ceros-esp"})
	if _, err := (Finder{SysRoot: f.root, MountInfo: "/nonexistent"}).First(); err != ErrNoStick {
		t.Fatalf("err %v", err)
	}
}

func TestUSBDevice(t *testing.T) {
	got := usbDevice("/sys/devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1.4/2-1.4:1.0/host6/target6:0:0/6:0:0:0")
	if got != "/sys/devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1.4" {
		t.Fatalf("got %q", got)
	}
}
