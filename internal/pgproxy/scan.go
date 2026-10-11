package pgproxy

import (
	"encoding/binary"
	"errors"
)

// After startup every Postgres message is a type byte, a four-byte length
// that counts itself, and a body.
const headerLen = 5

// A body can be a gigabyte (a bytea parameter, a COPY row). Anything that
// claims more is not Postgres.
const maxBodyLen = 1 << 30

var errFraming = errors.New("not a postgres message")

// tap is what a scanner reports to. keep says how many leading bytes of a
// message's body the tap wants to read (0 for none, negative for a type it
// doesn't know); message is then called once the whole message has gone by,
// with those bytes. body is only valid during the call.
type tap interface {
	keep(typ byte) int
	message(typ byte, body []byte, size int) error
}

// scanner follows one direction of a connection. The relay hands it every
// chunk it forwards; the scanner finds the message boundaries in them. It
// never holds a message back and never holds more of one than the tap asked
// for, so a DataRow or CopyData of any size costs a few compares.
type scanner struct {
	tap tap

	hdr  [headerLen]byte
	hdrN int

	// The message in flight, when it didn't arrive in one read.
	inBody bool
	typ    byte
	size   int    // length of the body
	remain int    // bytes of it still to come
	want   int    // bytes of it to keep
	buf    []byte // bytes kept so far
}

func (s *scanner) feed(p []byte) error {
	for {
		if !s.inBody {
			if len(p) == 0 {
				return nil
			}
			if s.hdrN == 0 && len(p) >= headerLen {
				typ := p[0]
				size, want, err := s.header(typ, p[1:headerLen])
				if err != nil {
					return err
				}
				p = p[headerLen:]
				// The usual case: the whole message is in this read, and
				// the tap can look at it where it lies.
				if len(p) >= size {
					if err := s.tap.message(typ, p[:want], size); err != nil {
						return err
					}
					p = p[size:]
					continue
				}
				s.begin(typ, size, want)
			} else {
				n := copy(s.hdr[s.hdrN:], p)
				s.hdrN += n
				p = p[n:]
				if s.hdrN < headerLen {
					return nil
				}
				s.hdrN = 0
				size, want, err := s.header(s.hdr[0], s.hdr[1:])
				if err != nil {
					return err
				}
				s.begin(s.hdr[0], size, want)
			}
		}
		take := min(len(p), s.remain)
		if need := s.want - len(s.buf); need > 0 {
			s.buf = append(s.buf, p[:min(need, take)]...)
		}
		s.remain -= take
		p = p[take:]
		if s.remain > 0 {
			return nil
		}
		s.inBody = false
		if err := s.tap.message(s.typ, s.buf, s.size); err != nil {
			return err
		}
	}
}

func (s *scanner) header(typ byte, length []byte) (size, want int, err error) {
	n := int(binary.BigEndian.Uint32(length))
	if n < 4 || n-4 > maxBodyLen {
		return 0, 0, errFraming
	}
	size = n - 4
	want = s.tap.keep(typ)
	if want < 0 {
		return 0, 0, errFraming
	}
	return size, min(want, size), nil
}

func (s *scanner) begin(typ byte, size, want int) {
	s.inBody = true
	s.typ, s.size, s.remain, s.want = typ, size, size, want
	s.buf = s.buf[:0]
}
