package repo

import (
	"bytes"
	"fmt"
	"io"
)

// The pkt-line framing git's protocols are written in (gitprotocol-common): four hexadecimal
// digits giving the length of the packet, those four included, then its data. Four lengths below
// four are packets of their own with no data: 0000 the flush, and, in protocol version 2, 0001
// the delimiter between the sections of a request and 0002 the end of a response.

const (
	// MaxPktLen is the longest a packet may be, its four digits included: git's LARGE_PACKET_MAX,
	// which gitprotocol-common says no implementation may exceed.
	MaxPktLen = 65520
	// MaxPktData is the most data one packet carries.
	MaxPktData = MaxPktLen - 4
)

// PktKind is what a packet is.
type PktKind int

// The kinds of packet.
const (
	// PktData carries data, which may be empty.
	PktData PktKind = iota
	// PktFlush, 0000, ends a message.
	PktFlush
	// PktDelim, 0001, separates the sections of a protocol version 2 request or response.
	PktDelim
	// PktResponseEnd, 0002, ends a protocol version 2 response where the connection stays open.
	PktResponseEnd
)

func (k PktKind) String() string {
	switch k {
	case PktData:
		return "data"
	case PktFlush:
		return "flush"
	case PktDelim:
		return "delim"
	case PktResponseEnd:
		return "response-end"
	}
	return fmt.Sprintf("PktKind(%d)", int(k))
}

// PktReader reads packets.
type PktReader struct {
	r   io.Reader
	buf [MaxPktLen]byte
}

// NewPktReader reads packets from r.
func NewPktReader(r io.Reader) *PktReader { return &PktReader{r: r} }

// Next reads one packet, answering its kind and, for a data packet, its data, which holds until
// the next call. A stream that ends between two packets answers io.EOF, and one that ends inside
// a packet io.ErrUnexpectedEOF.
func (p *PktReader) Next() (PktKind, []byte, error) {
	head := p.buf[:4]
	if _, err := io.ReadFull(p.r, head); err != nil {
		return 0, nil, err
	}
	n := 0
	for _, c := range head {
		// Git's own reader takes upper case as well, and so does this one: what git accepts from a
		// client this server accepts too.
		d := hexDigit(c)
		if d < 0 {
			return 0, nil, fmt.Errorf("repo: a packet length %q, which is not four hexadecimal digits", head)
		}
		n = n<<4 | d
	}
	switch {
	case n == 0:
		return PktFlush, nil, nil
	case n == 1:
		return PktDelim, nil, nil
	case n == 2:
		return PktResponseEnd, nil, nil
	case n < 4:
		return 0, nil, fmt.Errorf("repo: a packet of length %d, which is no packet", n)
	case n > MaxPktLen:
		return 0, nil, fmt.Errorf("repo: a packet of length %d, and a packet is at most %d", n, MaxPktLen)
	}
	data := p.buf[4:n]
	if _, err := io.ReadFull(p.r, data); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return PktData, data, nil
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// AppendPkt appends a data packet carrying data, and refuses more than MaxPktData.
func AppendPkt(b, data []byte) ([]byte, error) {
	if len(data) > MaxPktData {
		return b, fmt.Errorf("repo: a packet of %d bytes of data, and a packet carries at most %d", len(data), MaxPktData)
	}
	b = fmt.Appendf(b, "%04x", len(data)+4)
	return append(b, data...), nil
}

// WritePkt writes a data packet carrying data.
func WritePkt(w io.Writer, data []byte) error {
	b, err := AppendPkt(nil, data)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// WritePktString writes a data packet carrying s, which a line of the protocols ends with a line
// feed that s carries itself.
func WritePktString(w io.Writer, s string) error { return WritePkt(w, []byte(s)) }

// WriteFlush writes a flush packet, 0000.
func WriteFlush(w io.Writer) error { return writeSpecial(w, "0000") }

// WriteDelim writes a delimiter packet, 0001.
func WriteDelim(w io.Writer) error { return writeSpecial(w, "0001") }

// WriteResponseEnd writes a response-end packet, 0002.
func WriteResponseEnd(w io.Writer) error { return writeSpecial(w, "0002") }

func writeSpecial(w io.Writer, s string) error {
	_, err := io.WriteString(w, s)
	return err
}

// Side-band: in a response that multiplexes, each data packet begins with the band it belongs to:
// 1 the pack or the report, 2 progress for the person at the terminal, 3 an error that ends the
// response. Git prints band 2 prefixed with "remote: ", which is where a refusal is read.

// The bands.
const (
	BandData     = 1
	BandProgress = 2
	BandError    = 3
)

// The largest packets each form of side-band allows: side-band-64k's, the packet limit itself,
// and plain side-band's, which git's first multiplexed responses used.
const (
	SidebandMaxPkt      = MaxPktLen
	SidebandSmallMaxPkt = 1000
)

// NewSidebandWriter answers a writer framing what is written to it as data packets on band, each
// at most maxPkt bytes long with its four digits and its band. A write is sent as it is made,
// in as many packets as it needs.
func NewSidebandWriter(w io.Writer, band byte, maxPkt int) io.Writer {
	return &sidebandWriter{w: w, band: band, max: maxPkt - 5}
}

type sidebandWriter struct {
	w    io.Writer
	band byte
	max  int
	buf  []byte
}

func (s *sidebandWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n := min(len(b), s.max)
		s.buf = fmt.Appendf(s.buf[:0], "%04x", n+5)
		s.buf = append(s.buf, s.band)
		s.buf = append(s.buf, b[:n]...)
		if _, err := s.w.Write(s.buf); err != nil {
			return written, err
		}
		written += n
		b = b[n:]
	}
	return written, nil
}

// SidebandReader reads the data band of a side-band response until its flush, handing progress to
// Progress where it is set and ending with a *RemoteError on band 3.
type SidebandReader struct {
	pkts *PktReader
	// Progress is where band 2 goes; nil drops it.
	Progress io.Writer
	rest     []byte
	err      error
}

// NewSidebandReader reads a side-band response from packets.
func NewSidebandReader(pkts *PktReader) *SidebandReader { return &SidebandReader{pkts: pkts} }

// RemoteError is what the far end said on band 3.
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return "repo: the remote end said: " + e.Message }

// Read answers the data of band 1, and io.EOF at the flush that ends the response.
func (s *SidebandReader) Read(b []byte) (int, error) {
	for len(s.rest) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		kind, data, err := s.pkts.Next()
		switch {
		case err == io.EOF:
			s.err = io.ErrUnexpectedEOF
		case err != nil:
			s.err = err
		case kind == PktFlush:
			s.err = io.EOF
		case kind != PktData || len(data) == 0:
			s.err = fmt.Errorf("repo: a %s packet in a side-band response, where every packet names its band", kind)
		case data[0] == BandData:
			s.rest = data[1:]
		case data[0] == BandProgress:
			if s.Progress != nil {
				if _, err := s.Progress.Write(data[1:]); err != nil {
					s.err = err
				}
			}
		case data[0] == BandError:
			s.err = &RemoteError{Message: string(bytes.TrimRight(data[1:], "\n"))}
		default:
			s.err = fmt.Errorf("repo: a packet on band %d, and a side-band response has bands 1 to 3", data[0])
		}
	}
	n := copy(b, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}
