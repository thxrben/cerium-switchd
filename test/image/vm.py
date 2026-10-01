#!/usr/bin/env python3
"""Drives a cerOS disk image in QEMU/OVMF over its serial console
(docs/os-image.md; used by the image tests in this directory).

    vm.py start <disk.img> <workdir>      start the VM in the background
    vm.py run <workdir> '<shell command>' run a command in a Linux shell, print its output
    vm.py wait <workdir> <regex> [secs]   wait until the console shows regex (after the last mark)
    vm.py mark <workdir>                  later waits only look at console output after this point
    vm.py stop <workdir>                  power off (kill QEMU: like pulling the plug)
"""
import os, re, socket, subprocess, sys, time

OVMF = "/usr/share/edk2/x64"


def throttle():
    """VM_WRITE_BPS limits the disk's write rate (tests that cut the power
    in the middle of a write)."""
    bps = os.environ.get("VM_WRITE_BPS")
    return f",throttling.bps-write={bps}" if bps else ""


def start(disk, wd):
    os.makedirs(wd, exist_ok=True)
    vars_ = os.path.join(wd, "vars.fd")
    if not os.path.exists(vars_):
        subprocess.check_call(["cp", f"{OVMF}/OVMF_VARS.4m.fd", vars_])
    sock = os.path.join(wd, "serial.sock")
    for f in (sock, os.path.join(wd, "console.log"), os.path.join(wd, "mark")):
        if os.path.exists(f):
            os.remove(f)
    p = subprocess.Popen([
        "qemu-system-x86_64", "-enable-kvm", "-m", "2048", "-smp", "2", "-machine", "q35",
        "-drive", f"if=pflash,format=raw,readonly=on,file={OVMF}/OVMF_CODE.4m.fd",
        "-drive", f"if=pflash,format=raw,file={vars_}",
        "-drive", f"file={disk},format=raw,if=virtio" + throttle(),
        "-device", "i6300esb", "-action", "watchdog=reset",
        "-nic", "user,model=virtio-net-pci", "-nic", "user,model=virtio-net-pci",
        "-display", "none", "-monitor", "none",
        "-chardev", f"socket,id=s0,path={sock},server=on,wait=off,logfile={wd}/console.log",
        "-serial", "chardev:s0"], stdout=open(os.path.join(wd, "qemu.log"), "w"), stderr=subprocess.STDOUT,
        start_new_session=True)
    open(os.path.join(wd, "qemu.pid"), "w").write(str(p.pid))
    for _ in range(50):
        if os.path.exists(sock):
            return
        time.sleep(0.1)
    sys.exit("QEMU did not start: " + open(os.path.join(wd, "qemu.log")).read())


def log(wd):
    try:
        return open(os.path.join(wd, "console.log"), errors="replace").read()
    except FileNotFoundError:
        return ""


ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[=>()][A-Z0-9]?|\r")


def mark_pos(wd):
    try:
        return int(open(os.path.join(wd, "mark")).read())
    except (FileNotFoundError, ValueError):
        return 0


def wait(wd, pattern, secs=180, since=None):
    if since is None:
        since = mark_pos(wd)
    rx = re.compile(pattern)
    end = time.time() + secs
    while time.time() < end:
        text = ANSI.sub("", log(wd)[since:])
        if rx.search(text):
            return text
        time.sleep(0.5)
    sys.stderr.write(ANSI.sub("", log(wd)[since:])[-3000:])
    sys.exit(f"timeout waiting for {pattern!r}")


def send(wd, data):
    """Types data slowly: the serial line has no flow control and the CLI's
    line editor drops input that arrives faster than it reads."""
    s = socket.socket(socket.AF_UNIX)
    s.connect(os.path.join(wd, "serial.sock"))
    b = data.encode()
    for i in range(0, len(b), 8):
        s.sendall(b[i:i + 8])
        time.sleep(0.02)
    s.close()


def last_line(wd):
    return ANSI.sub("", log(wd)).rstrip(" ").split("\n")[-1]


def run(wd, cmd, secs=120):
    """Runs cmd in a root shell. The console lands in the CLI: 'start shell'
    leads to bash; the shell stays open between calls."""
    tag = "%08x" % int(time.time() * 1000 % 0xffffffff)
    if "__SH__" not in last_line(wd):
        send(wd, "\n")
        time.sleep(1)
        if "__SH__" not in last_line(wd):
            send(wd, "start shell\n")
            time.sleep(3)
            send(wd, "export PS1=__SH__' '; stty -echo cols 250; export SYSTEMD_PAGER= PAGER=cat\n")
            time.sleep(1)
    pos = len(log(wd))
    send(wd, f"echo BEGIN{tag}; {cmd}; echo END{tag} $?\n")
    text = wait(wd, f"END{tag} \\d+", secs, pos)
    m = re.search(f"BEGIN{tag}\n(.*)END{tag} (\\d+)", text, re.S)
    if not m:
        return text, 1
    return m.group(1), int(m.group(2))


def stop(wd):
    try:
        pid = int(open(os.path.join(wd, "qemu.pid")).read())
        os.kill(pid, 9)
    except (FileNotFoundError, ProcessLookupError, ValueError):
        pass


if __name__ == "__main__":
    a = sys.argv[1:]
    if a[0] == "start":
        start(a[1], a[2])
    elif a[0] == "run":
        out, rc = run(a[1], a[2], int(a[3]) if len(a) > 3 else 120)
        sys.stdout.write(out)
        sys.exit(rc)
    elif a[0] == "wait":
        wait(a[1], a[2], int(a[3]) if len(a) > 3 else 180)
    elif a[0] == "mark":
        open(os.path.join(a[1], "mark"), "w").write(str(len(log(a[1]))))
    elif a[0] == "stop":
        stop(a[1])
