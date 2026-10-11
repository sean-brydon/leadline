//go:build unix

package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// How long output is still read after the command itself has exited, for a
// process it left behind that keeps printing. A shell would have returned
// to its prompt already.
const afterExit = 250 * time.Millisecond

// running is a started command and the plumbing around it.
type running struct {
	cmd *exec.Cmd
	pid int
	// The pseudo-terminal, when the command has one. The parent keeps its
	// own handle on the terminal's far side until the output is drained:
	// on macOS the last close throws away what has not been read yet.
	master, slave *os.File
	// Pipes for the streams that are not the terminal; nil when unused.
	outR, errR *os.File
	// The terminal this process was started in, for its size.
	real *os.File

	stdio stdio
	sink  func(stream Stream, at time.Time, p []byte)

	// Set when the command has exited; wake is written to at the same time
	// to get the pumps out of select.
	exited       atomic.Bool
	wakeR, wakeW *os.File
	pumps        sync.WaitGroup

	// The terminal's settings from before raw mode. Guarded by mu, because
	// the wait loop, the signal loop and the exit path all restore.
	mu     sync.Mutex
	cooked *term.State
	// The terminal sends Ctrl-C to the command itself (they share a process
	// group), so this process must not send it a second time.
	terminalSignals bool
	signals         chan os.Signal
	// Closed when forwardSignals has returned.
	forwarded chan struct{}
}

// stdio is where the command's streams go.
type stdio struct {
	in, out, err *os.File
}

// StreamIDs names the streams the command writes to, as the Node provider
// will find them with fstat: "dev:ino" for stdout and for stderr. The
// provider marks its console lines only on a stream listed here, so a
// process whose output goes somewhere else (`node x.js > out.json`) writes
// exactly what it would have.
func (r *running) streamIDs() string {
	ids := ""
	for _, f := range []*os.File{r.cmd.Stdout.(*os.File), r.cmd.Stderr.(*os.File)} {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			continue
		}
		if ids != "" {
			ids += ","
		}
		ids += fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return ids
}

// prepare sets up the command's streams: a pseudo-terminal when this
// process's stdout is a terminal, so the command behaves as it does when
// run directly (colours, prompts, single keys, window size), and plain
// pipes otherwise. It does not start the command.
func prepare(cmd *exec.Cmd, std stdio, sink func(Stream, time.Time, []byte)) (*running, error) {
	r := &running{cmd: cmd, stdio: std, sink: sink}
	var err error
	if r.wakeR, r.wakeW, err = os.Pipe(); err != nil {
		return nil, err
	}

	if !term.IsTerminal(int(std.out.Fd())) {
		// Not a terminal (CI, a pipe, a file): plain pipes, each stream to
		// its own place, and stdin left exactly as it is.
		if r.outR, cmd.Stdout, err = pipe(); err != nil {
			return nil, err
		}
		if r.errR, cmd.Stderr, err = pipe(); err != nil {
			return nil, err
		}
		cmd.Stdin = std.in
		r.terminalSignals = sharesTerminal(std)
		return r, nil
	}

	r.real = std.out
	if r.master, r.slave, err = pty.Open(); err != nil {
		return nil, err
	}
	if size, err := pty.GetsizeFull(r.real); err == nil {
		_ = pty.Setsize(r.master, size)
	}
	// The same line settings the real terminal has now (erase key, flow
	// control and the rest), as the command would have found them.
	if state, err := term.GetState(int(r.real.Fd())); err == nil {
		_ = term.Restore(int(r.slave.Fd()), state)
	}

	cmd.Stdout = r.slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
	if term.IsTerminal(int(std.in.Fd())) {
		cmd.Stdin = r.slave
		cmd.SysProcAttr.Ctty = 0
	} else {
		// Input that is a file or a pipe stays one: the command must see
		// its end, which a terminal cannot carry.
		cmd.Stdin = std.in
	}
	if term.IsTerminal(int(std.err.Fd())) {
		cmd.Stderr = r.slave
	} else if r.errR, cmd.Stderr, err = pipe(); err != nil {
		// `2>errors.log` still gets the errors, and only them.
		return nil, err
	}
	return r, nil
}

func pipe() (r *os.File, w *os.File, err error) {
	return os.Pipe()
}

// start runs the command and begins copying its output.
func (r *running) start() error {
	// Signals are taken before the command exists, so one that arrives in
	// between waits in the channel and is passed on, not acted on here.
	// SIGPIPE is taken so a closed stdout (`| head`) is an error from write
	// and not the end of this process; see pump. Taken, not ignored: an
	// ignored signal would be inherited by the command.
	r.signals = make(chan os.Signal, 32)
	signal.Notify(r.signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGPIPE)
	if err := r.cmd.Start(); err != nil {
		signal.Stop(r.signals)
		r.closeAll()
		return err
	}
	r.pid = r.cmd.Process.Pid
	r.forwarded = make(chan struct{})
	go r.forwardSignals()

	if r.master != nil {
		if r.cmd.Stdin == r.slave {
			// Raw mode: every key goes to the command's terminal untouched,
			// which then does the echoing, the line editing and the Ctrl-C.
			r.raw()
			go func() { _, _ = io.Copy(r.master, r.stdio.in) }()
		}
		r.pump(r.master, r.stdio.out, Terminal)
	} else {
		// The write end is the command's now; holding it would hide its EOF.
		r.cmd.Stdout.(*os.File).Close()
		r.pump(r.outR, r.stdio.out, Stdout)
	}
	if r.errR != nil {
		r.cmd.Stderr.(*os.File).Close()
		r.pump(r.errR, r.stdio.err, Stderr)
	}
	return nil
}

// wait returns when the command has ended and its output has been passed
// on: the exit code to leave with, and the signal's name if one ended it.
func (r *running) wait() (code int, sig string) {
	defer r.finish()
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(r.pid, &status, syscall.WUNTRACED, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil:
			return 1, ""
		case status.Exited():
			return status.ExitStatus(), ""
		case status.Signaled():
			// What a shell reports for a command a signal ended.
			return 128 + int(status.Signal()), unix.SignalName(status.Signal())
		case status.Stopped():
			r.suspend()
		}
	}
}

func (r *running) finish() {
	r.exited.Store(true)
	_, _ = r.wakeW.Write([]byte{0})
	r.pumps.Wait()
	// After Stop nothing more is sent, so the channel can be closed, and the
	// loop reading it is over before the files it uses are.
	signal.Stop(r.signals)
	close(r.signals)
	<-r.forwarded
	r.restore()
	_ = r.cmd.Process.Release()
	r.closeAll()
}

func (r *running) closeAll() {
	for _, f := range []*os.File{r.slave, r.master, r.outR, r.errR, r.wakeR, r.wakeW} {
		if f != nil {
			f.Close()
		}
	}
}

// pump copies one of the command's streams to where it would have gone,
// and hands a copy of each read to the sink. The copy to the terminal comes
// first and is the only thing that can take time.
func (r *running) pump(src, dst *os.File, stream Stream) {
	r.pumps.Add(1)
	fd, wake := int(src.Fd()), int(r.wakeR.Fd())
	go func() {
		defer r.pumps.Done()
		var marks markFilter
		buf := make([]byte, 32<<10)
		var deadline time.Time
		for {
			ready, woken := waitReadable(fd, wake, deadline)
			if !ready {
				if woken || r.exited.Load() {
					break
				}
				continue
			}
			if woken && deadline.IsZero() {
				deadline = time.Now().Add(afterExit)
			}
			n, err := unix.Read(fd, buf)
			if n > 0 {
				at := time.Now()
				var werr error
				if dst != nil {
					if _, werr = dst.Write(marks.Strip(buf[:n])); werr != nil {
						dst = nil
					}
				}
				r.sink(stream, at, buf[:n])
				if werr != nil && stream != Terminal {
					// Nobody is reading (`| head` has had enough). Run
					// directly, the command would learn that from its next
					// write; closing our end tells it the same. What it
					// had already printed is still a log.
					src.Close()
					return
				}
			}
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			// End of file, or EIO from a terminal whose far side is gone.
			if n <= 0 || err != nil {
				break
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				break
			}
		}
		if dst != nil {
			_, _ = dst.Write(marks.Flush())
		}
	}()
}

// waitReadable blocks until fd has something to read (ready), or the wake
// pipe has (woken: the command has exited). Both can be true: output that
// was written before the exit is still to be read. Past the deadline it
// stops waiting.
func waitReadable(fd, wake int, deadline time.Time) (ready, woken bool) {
	for {
		var set unix.FdSet
		set.Zero()
		set.Set(fd)
		set.Set(wake)
		var timeout *unix.Timeval
		if !deadline.IsZero() {
			tv := unix.NsecToTimeval(max(0, time.Until(deadline)).Nanoseconds())
			timeout = &tv
		}
		n, err := unix.Select(max(fd, wake)+1, &set, nil, nil, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n == 0 {
			return false, true
		}
		woken = set.IsSet(wake)
		if set.IsSet(fd) {
			return true, woken
		}
		if woken {
			// Exited, and nothing was readable in the same look.
			return false, true
		}
	}
}

func (r *running) forwardSignals() {
	defer close(r.forwarded)
	for sig := range r.signals {
		s := sig.(syscall.Signal)
		switch s {
		case syscall.SIGPIPE:
		case syscall.SIGWINCH:
			if r.master != nil {
				// The kernel tells the command's foreground job itself.
				_ = pty.InheritSize(r.real, r.master)
			}
		case syscall.SIGINT, syscall.SIGQUIT:
			if r.terminalSignals {
				continue
			}
			r.signal(s)
		default:
			r.signal(s)
		}
	}
}

func (r *running) signal(s syscall.Signal) {
	if !r.exited.Load() {
		_ = syscall.Kill(r.pid, s)
	}
}

// raw puts the real terminal in raw mode and remembers how it was.
func (r *running) raw() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooked != nil {
		return
	}
	if state, err := term.MakeRaw(int(r.stdio.in.Fd())); err == nil {
		r.cooked = state
	}
}

// restore puts the real terminal back as it was. It is safe to call from
// anywhere, any number of times.
func (r *running) restore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooked != nil {
		_ = term.Restore(int(r.stdio.in.Fd()), r.cooked)
		r.cooked = nil
	}
}

func (r *running) isRaw() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cooked != nil
}

// suspend is Ctrl-Z. In raw mode the key went to the command's terminal,
// which stopped the command; the shell that started this process knows
// nothing of it yet. Stopping this process too hands the real terminal
// back to the shell, and `fg` continues both.
func (r *running) suspend() {
	if !r.isRaw() {
		// A shared process group was stopped as one by the terminal, or
		// someone stopped the command alone. Either way it is not ours to undo.
		return
	}
	r.restore()
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
	// Continued.
	r.raw()
	_ = pty.InheritSize(r.real, r.master)
	_ = syscall.Kill(-r.pid, syscall.SIGCONT)
}

// sharesTerminal reports whether a terminal's Ctrl-C reaches the command
// directly: there is a controlling terminal and this process, whose group
// the command joins, is its foreground job.
func sharesTerminal(std stdio) bool {
	for _, f := range []*os.File{std.in, std.out, std.err} {
		if term.IsTerminal(int(f.Fd())) {
			return foreground(int(f.Fd()))
		}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	defer tty.Close()
	return foreground(int(tty.Fd()))
}

func foreground(fd int) bool {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	return err == nil && pgrp == syscall.Getpgrp()
}
