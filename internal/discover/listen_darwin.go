//go:build darwin && cgo

package discover

/*
#include <errno.h>
#include <libproc.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>

typedef struct {
	int port;
	int family;
	unsigned char addr[16];
} ll_listen;

// ll_listening fills out for a file descriptor that is a listening TCP
// socket and returns 1; 0 for anything else, -1 when it can't be asked.
static int ll_listening(int pid, int fd, ll_listen *out) {
	struct socket_fdinfo si;
	int n = proc_pidfdinfo(pid, fd, PROC_PIDFDSOCKETINFO, &si, sizeof(si));
	if (n < (int)sizeof(si)) return -1;
	if (si.psi.soi_kind != SOCKINFO_TCP) return 0;
	if (si.psi.soi_proto.pri_tcp.tcpsi_state != TSI_S_LISTEN) return 0;
	struct in_sockinfo *in = &si.psi.soi_proto.pri_tcp.tcpsi_ini;
	out->port = ntohs((unsigned short)in->insi_lport);
	out->family = si.psi.soi_family;
	memset(out->addr, 0, sizeof(out->addr));
	if (si.psi.soi_family == AF_INET) {
		memcpy(out->addr, &in->insi_laddr.ina_46.i46a_addr4, 4);
	} else {
		memcpy(out->addr, &in->insi_laddr.ina_6, 16);
	}
	return 1;
}

// ll_fds lists a process's file descriptors into buf and returns how many
// bytes it wrote, or -1 with errno.
static int ll_fds(int pid, void *buf, int size) {
	errno = 0;
	int n = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, buf, size);
	if (n <= 0) return -1;
	return n;
}

typedef struct {
	int ppid;
	unsigned int uid;
	long long start_sec;
	char comm[MAXCOMLEN + 1];
} ll_proc;

static int ll_procinfo(int pid, ll_proc *out) {
	struct proc_bsdinfo bi;
	int n = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bi, sizeof(bi));
	if (n < (int)sizeof(bi)) {
		// Another user's process: the short form is still given.
		struct proc_bsdshortinfo si;
		n = proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &si, sizeof(si));
		if (n < (int)sizeof(si)) return -1;
		out->ppid = si.pbsi_ppid;
		out->uid = si.pbsi_uid;
		out->start_sec = 0;
		strlcpy(out->comm, si.pbsi_comm, sizeof(out->comm));
		return 0;
	}
	out->ppid = bi.pbi_ppid;
	out->uid = bi.pbi_uid;
	out->start_sec = (long long)bi.pbi_start_tvsec;
	strlcpy(out->comm, bi.pbi_name[0] ? bi.pbi_name : bi.pbi_comm, sizeof(out->comm));
	return 0;
}

static int ll_cwd(int pid, char *out, int size) {
	struct proc_vnodepathinfo vi;
	int n = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &vi, sizeof(vi));
	if (n < (int)sizeof(vi)) return -1;
	strlcpy(out, vi.pvi_cdir.vip_path, size);
	return 0;
}
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"net"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwin asks libproc, the library `lsof` and Activity Monitor are built
// on. It needs no privilege for this user's processes and shows nothing
// of anyone else's sockets.
type darwin struct{}

func hostSystem() system { return darwin{} }

func (darwin) sockets() ([]rawSocket, Limits) {
	limits := Limits{Method: "libproc"}
	n := int(C.proc_listallpids(nil, 0))
	if n <= 0 {
		return nil, limits
	}
	pids := make([]C.int, n+64)
	n = int(C.proc_listallpids(unsafe.Pointer(&pids[0]), C.int(len(pids)*4)))
	if n <= 0 {
		return nil, limits
	}
	if n > len(pids) {
		n = len(pids)
	}

	var out []rawSocket
	fds := make([]byte, 64<<10)
	for _, pid := range pids[:n] {
		if pid <= 0 {
			continue
		}
		got, err := C.ll_fds(pid, unsafe.Pointer(&fds[0]), C.int(len(fds)))
		if got < 0 {
			// EPERM is another user's process. ESRCH is one that ended
			// while we looked, which is nobody's loss.
			if err == syscall.EPERM {
				limits.SkippedProcesses++
			}
			continue
		}
		// Each entry is struct proc_fdinfo: the descriptor, then its type.
		for off := 0; off+8 <= int(got); off += 8 {
			fd := int32(binary.NativeEndian.Uint32(fds[off:]))
			if binary.NativeEndian.Uint32(fds[off+4:]) != C.PROX_FDTYPE_SOCKET {
				continue
			}
			var l C.ll_listen
			if C.ll_listening(pid, C.int(fd), &l) != 1 {
				continue
			}
			size := 16
			if l.family == C.AF_INET {
				size = 4
			}
			ip := make(net.IP, size)
			for i := range ip {
				ip[i] = byte(l.addr[i])
			}
			out = append(out, rawSocket{pid: int(pid), port: int(l.port), ip: ip})
		}
	}
	limits.Complete = limits.SkippedProcesses == 0
	if !limits.Complete {
		limits.Note = "Processes that belong to another user (system services, a database started as root) are not listed: macOS shows a process's sockets only to its owner."
	}
	return out, limits
}

func (darwin) process(pid int) (procInfo, bool) {
	var p C.ll_proc
	if C.ll_procinfo(C.int(pid), &p) != 0 {
		return procInfo{}, false
	}
	info := procInfo{pid: pid, ppid: int(p.ppid), name: C.GoString(&p.comm[0])}
	if p.start_sec > 0 {
		info.started = time.Unix(int64(p.start_sec), 0)
	}
	var path [C.PROC_PIDPATHINFO_MAXSIZE]C.char
	if C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.PROC_PIDPATHINFO_MAXSIZE) > 0 {
		info.exe = C.GoString(&path[0])
	}
	var cwd [C.MAXPATHLEN]C.char
	if C.ll_cwd(C.int(pid), &cwd[0], C.MAXPATHLEN) == 0 {
		info.cwd = C.GoString(&cwd[0])
	}
	info.args = procArgs(pid)
	return info, true
}

// procArgs reads a process's arguments from kern.procargs2. That block
// is: the argument count, the executable's path, padding, the arguments,
// and then the environment. Reading stops after the last argument and the
// block is wiped: the environment is the one thing in it that is nobody's
// business here.
func procArgs(pid int) []string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil
	}
	defer clear(raw)
	argc := int(binary.NativeEndian.Uint32(raw))
	rest := raw[4:]
	// The executable's path, and the NULs that pad it.
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil
	}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	var args []string
	for i := 0; i < argc && len(rest) > 0; i++ {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		args = append(args, string(rest[:end]))
		if end >= len(rest) {
			break
		}
		rest = rest[end+1:]
	}
	return args
}
