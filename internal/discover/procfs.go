package discover

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procfs reads what is listening from Linux's /proc. It has no build tag,
// so its parsing is tested everywhere against a made-up tree; only
// listen_linux.go points it at the real /proc.
//
// /proc/net/tcp lists every listening socket in this network namespace
// with its inode, whoever owns it. Which process holds an inode is in
// /proc/<pid>/fd, and that is readable only for this user's processes: so
// another user's listening port is seen, and its process is not.
type procfs struct {
	root string
	// userHZ is the unit of a process's start time in /proc/<pid>/stat.
	// It is 100 on every Linux Go runs on.
	userHZ int64
}

func (p procfs) sockets() ([]rawSocket, Limits) {
	limits := Limits{Method: "procfs"}
	type listening struct {
		port int
		ip   net.IP
		uid  int
	}
	byInode := map[string]listening{}
	var order []string
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(filepath.Join(p.root, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // the header
		for sc.Scan() {
			// sl local_address rem_address st tx rx tr tm->when retrnsmt uid timeout inode ...
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" { // 0A is LISTEN
				continue
			}
			ip, port, ok := procAddr(fields[1])
			if !ok {
				continue
			}
			uid, _ := strconv.Atoi(fields[7])
			inode := fields[9]
			if _, seen := byInode[inode]; !seen {
				order = append(order, inode)
			}
			byInode[inode] = listening{port: port, ip: ip, uid: uid}
		}
		f.Close()
	}
	if len(byInode) == 0 {
		limits.Complete = true
		return nil, limits
	}

	// Which process has each inode open.
	var out []rawSocket
	owned := map[string]bool{}
	entries, _ := os.ReadDir(p.root)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join(p.root, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				limits.SkippedProcesses++
			}
			continue
		}
		denied := false
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				// Root without CAP_SYS_PTRACE (in a container, say) may
				// list another user's descriptors and not read them.
				if errors.Is(err, fs.ErrPermission) && !denied {
					denied = true
					limits.SkippedProcesses++
				}
				continue
			}
			inode, ok := strings.CutPrefix(link, "socket:[")
			if !ok {
				continue
			}
			inode = strings.TrimSuffix(inode, "]")
			l, ok := byInode[inode]
			if !ok {
				continue
			}
			owned[inode] = true
			out = append(out, rawSocket{pid: pid, port: l.port, ip: l.ip, uid: l.uid})
		}
	}
	for _, inode := range order {
		if owned[inode] {
			continue
		}
		l := byInode[inode]
		limits.UnownedPorts++
		out = append(out, rawSocket{port: l.port, ip: l.ip, uid: l.uid})
	}
	limits.Complete = limits.UnownedPorts == 0
	if !limits.Complete {
		limits.Note = "Some ports are listed without a process: they belong to another user (root's services, Docker's published ports), and Linux shows which process holds a socket only to its owner."
	}
	return out, limits
}

// procAddr reads /proc/net/tcp's address: the IP as hex in host-order
// 32-bit words, a colon, the port as hex.
func procAddr(s string) (net.IP, int, bool) {
	addr, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, false
	}
	// Each word was printed as a number, so on a little-endian machine
	// (every one this runs on) its bytes are reversed.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw), int(port), true
}

func (p procfs) process(pid int) (procInfo, bool) {
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procInfo{}, false
	}
	info := procInfo{pid: pid}
	// pid (comm) state ppid ... with comm free to hold spaces and brackets,
	// so the fields are counted from the last ")".
	if open, shut := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')'); open >= 0 && shut > open {
		info.name = string(stat[open+1 : shut])
		fields := strings.Fields(string(stat[shut+1:]))
		if len(fields) > 1 {
			info.ppid, _ = strconv.Atoi(fields[1])
		}
		// starttime is field 22 of the line, 20th after the name: in
		// ticks since the machine started.
		if len(fields) > 19 {
			if ticks, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
				if boot := p.bootTime(); !boot.IsZero() {
					hz := p.userHZ
					if hz <= 0 {
						hz = 100
					}
					info.started = boot.Add(time.Duration(ticks) * time.Second / time.Duration(hz))
				}
			}
		}
	}
	// These three are the owner's to read; for another user's process
	// they stay empty.
	if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil && len(cmdline) > 0 {
		info.args = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		info.exe = strings.TrimSuffix(exe, " (deleted)")
	}
	if cwd, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
		info.cwd = cwd
	}
	return info, true
}

// bootTime is when the machine started: btime in /proc/stat.
func (p procfs) bootTime() time.Time {
	f, err := os.Open(filepath.Join(p.root, "stat"))
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			if sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(sec, 0)
			}
		}
	}
	return time.Time{}
}
