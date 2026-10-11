package runner

import (
	"sync/atomic"
	"time"
)

// How long a stream must be quiet before a group in progress is taken to be
// complete. A stack trace is printed in one go; a pause this long means the
// next line is something else.
const groupIdle = 80 * time.Millisecond

type chunk struct {
	stream Stream
	at     time.Time
	data   []byte
}

// capture is everything that is done with the command's output apart from
// showing it: lines, groups, levels, and off to the exporter. It runs on
// its own goroutine, fed through a channel that Offer never waits on.
type capture struct {
	chunks  chan chunk
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	sink    func(at time.Time, stream Stream, e Entry)

	streams map[Stream]*streamState
}

type streamState struct {
	lines  assembler
	groups grouper
	// When the line being handled was read.
	at time.Time
}

// newCapture starts capturing. sink gets each finished event; it is called
// from the capture goroutine only.
func newCapture(sink func(at time.Time, stream Stream, e Entry)) *capture {
	c := &capture{
		chunks:  make(chan chunk, 1024),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		sink:    sink,
		streams: map[Stream]*streamState{},
	}
	go c.run()
	return c
}

// Offer hands over a copy of what was just read. If capture has fallen
// behind it is dropped and counted: the command's output is never held up
// for Leadline's sake.
func (c *capture) Offer(stream Stream, at time.Time, p []byte) {
	if len(p) == 0 {
		return
	}
	select {
	case c.chunks <- chunk{stream, at, append([]byte(nil), p...)}:
	default:
		c.dropped.Add(int64(len(p)))
	}
}

// Close handles what is queued, finishes the last lines and groups, and
// returns when the sink has had everything.
func (c *capture) Close() {
	close(c.stop)
	<-c.done
}

func (c *capture) run() {
	defer close(c.done)
	// A bug in here must cost the capture, never the command: the pumps
	// keep offering, the channel fills, and the output still flows.
	defer func() { _ = recover() }()

	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	for {
		select {
		case ch := <-c.chunks:
			c.handle(ch)
			idle.Reset(groupIdle)
		case <-idle.C:
			for _, s := range c.streams {
				s.groups.Flush()
			}
		case <-c.stop:
			for {
				select {
				case ch := <-c.chunks:
					c.handle(ch)
					continue
				default:
				}
				break
			}
			for _, s := range c.streams {
				s.lines.Close()
				s.groups.Flush()
			}
			return
		}
	}
}

func (c *capture) handle(ch chunk) {
	s := c.streams[ch.stream]
	if s == nil {
		s = &streamState{}
		stream := ch.stream
		s.groups.emit = func(at time.Time, lines []string) {
			c.sink(at, stream, Classify(lines, stream))
		}
		s.lines.emit = func(text string, sent bool) {
			if sent {
				// The provider's own record of this line is on its way. It
				// also ends whatever came before it.
				s.groups.Flush()
				return
			}
			s.groups.Line(s.at, text)
			// Lines that came in one read are still in an order; a
			// nanosecond apart, they keep it wherever they are sorted.
			s.at = s.at.Add(time.Nanosecond)
		}
		c.streams[ch.stream] = s
	}
	if ch.at.After(s.at) {
		s.at = ch.at
	}
	s.lines.Write(ch.data)
}
