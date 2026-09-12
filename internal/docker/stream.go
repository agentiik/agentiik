package docker

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// StdStream is which of a container's streams a frame came from. The daemon writes the
// number in the first byte of every frame header, and these are the three it uses.
type StdStream int

const (
	Stdin  StdStream = 0
	Stdout StdStream = 1
	Stderr StdStream = 2
)

// String names the stream as a person reads it.
func (s StdStream) String() string {
	switch s {
	case Stdin:
		return "stdin"
	case Stdout:
		return "stdout"
	case Stderr:
		return "stderr"
	default:
		return fmt.Sprintf("stream %d", int(s))
	}
}

// frameHeader is the eight bytes in front of every frame: the stream in the first byte,
// three bytes of padding, then the length as a big-endian uint32.
const frameHeader = 8

// maxFrame bounds one frame. The daemon writes at most 32 KiB in one, and a header
// claiming more than this is a stream that has lost its alignment rather than a large
// write, so it is refused instead of being allocated.
const maxFrame = 1 << 20

// Frame is one write by the container, on one of its streams.
//
// Bytes belongs to the caller: each frame carries its own slice rather than a window
// onto a buffer the next call overwrites, because what happens to these bytes is a
// masker, a log and a store, and a slice that changes underneath any of them is the kind
// of bug that shows up as a secret in a log once a month.
type Frame struct {
	Stream StdStream
	Bytes  []byte
}

// Stream is a container's output, demultiplexed, with its standard input where there is
// one.
//
// Keeping the two output streams apart is what the whole of this file is for. Standard
// output is the shorthand a script publishes on out, and standard error is the log that
// goes through the secret masker; a pseudo-terminal would merge them into one, which is
// why Config.Tty is false and stays false.
//
// Stdin is nil on a stream that carries none, which is what logs is.
type Stream struct {
	Stdin io.WriteCloser

	r      *bufio.Reader
	closer io.Closer
	header [frameHeader]byte
}

// newStream wraps a reader as a demultiplexed stream.
func newStream(r io.Reader, stdin io.WriteCloser, closer io.Closer) *Stream {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return &Stream{Stdin: stdin, r: br, closer: closer}
}

// Next reads one frame, and answers io.EOF at the end of the stream.
//
// A stream that ends in the middle of a frame is reported as what it is rather than as a
// clean end: a container whose output was truncated is not a container that wrote
// nothing, and the difference decides whether a log is short or a log is wrong.
func (s *Stream) Next() (Frame, error) {
	if _, err := io.ReadFull(s.r, s.header[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return Frame{}, fmt.Errorf("the container stream ended inside a frame header: %w", err)
		}
		return Frame{}, err
	}

	stream := StdStream(s.header[0])
	size := binary.BigEndian.Uint32(s.header[4:])
	if size > maxFrame {
		return Frame{}, fmt.Errorf("the container stream announced a frame of %d bytes, which is past the %d a frame can be: the stream has lost its alignment", size, maxFrame)
	}
	if size == 0 {
		return Frame{Stream: stream}, nil
	}

	b := make([]byte, size)
	if _, err := io.ReadFull(s.r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, fmt.Errorf("the container stream ended inside a frame of %d bytes: %w", size, err)
	}
	return Frame{Stream: stream, Bytes: b}, nil
}

// CloseWrite closes the writing half and leaves the reading half open.
//
// It is how the envelope on standard input ends. A brick reading standard input to the
// end needs an end, and closing the whole connection to give it one would throw away the
// output the brick is about to write.
func (s *Stream) CloseWrite() error {
	if s.Stdin == nil {
		return nil
	}
	return s.Stdin.Close()
}

// Close releases the stream. On an attach it closes the hijacked connection, which never
// goes back to the pool.
func (s *Stream) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}
