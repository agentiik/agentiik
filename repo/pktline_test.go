package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"
)

// packets reads every packet of b, answering each as the text a test compares: the data of a data
// packet, and the name of any other in brackets.
func packets(t *testing.T, b []byte) []string {
	t.Helper()
	var got []string
	r := NewPktReader(bytes.NewReader(b))
	for {
		kind, data, err := r.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("after %q: %s", got, err)
		}
		if kind == PktData {
			got = append(got, string(data))
		} else {
			got = append(got, "["+kind.String()+"]")
		}
	}
}

func TestPacketsReadBackAsTheyWereWritten(t *testing.T) {
	var b bytes.Buffer
	WritePktString(&b, "command=ls-refs\n")
	WriteDelim(&b)
	WritePkt(&b, nil)
	WritePkt(&b, bytes.Repeat([]byte{'x'}, MaxPktData))
	WriteFlush(&b)
	WriteResponseEnd(&b)
	want := []string{"command=ls-refs\n", "[delim]", "", strings.Repeat("x", MaxPktData), "[flush]", "[response-end]"}
	got := packets(t, b.Bytes())
	if len(got) != len(want) {
		t.Fatalf("read %d packets, and %d were written", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("packet %d reads as %.40q, and was written as %.40q", i, got[i], want[i])
		}
	}
	if !strings.HasPrefix(b.String(), "0014command=ls-refs\n00010004fff0xxx") || !strings.HasSuffix(b.String(), "xxx00000002") {
		t.Errorf("the packets are written as %.40q", b.String())
	}
	if err := WritePkt(io.Discard, make([]byte, MaxPktData+1)); err == nil {
		t.Error("a packet of more than MaxPktData is written")
	}
	if got := packets(t, []byte("000AHELLO!0000")); got[0] != "HELLO!" || got[1] != "[flush]" {
		t.Errorf("a length in upper case, which git reads, reads as %q", got)
	}
}

func TestAPacketThatIsNoneIsRefused(t *testing.T) {
	for name, in := range map[string]string{
		"a length that is not hexadecimal":   "00zz",
		"a length of three":                  "0003",
		"a length past the largest packet":   "fff1" + strings.Repeat("x", 0xfff1),
		"a packet that ends inside its data": "0010short",
		"a stream that ends inside a length": "00",
	} {
		_, _, err := NewPktReader(strings.NewReader(in)).Next()
		if err == nil || err == io.EOF {
			t.Errorf("%s reads with %v", name, err)
		}
	}
}

func TestSidebandCarriesDataProgressAndErrors(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 20000)
	for _, max := range []int{SidebandMaxPkt, SidebandSmallMaxPkt} {
		var b bytes.Buffer
		NewSidebandWriter(&b, BandProgress, max).Write([]byte("Counting objects\n"))
		if n, err := NewSidebandWriter(&b, BandData, max).Write(data); n != len(data) || err != nil {
			t.Fatalf("wrote %d of %d bytes: %v", n, len(data), err)
		}
		WriteFlush(&b)
		pkts := NewPktReader(bytes.NewReader(b.Bytes()))
		for {
			kind, p, err := pkts.Next()
			if err == io.EOF {
				break
			}
			if kind == PktData && len(p)+4 > max {
				t.Fatalf("a packet of %d bytes where the most is %d", len(p)+4, max)
			}
		}
		var progress bytes.Buffer
		r := NewSidebandReader(NewPktReader(bytes.NewReader(b.Bytes())))
		r.Progress = &progress
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("read %d bytes of the %d written on band 1: %v", len(got), len(data), err)
		}
		if progress.String() != "Counting objects\n" {
			t.Errorf("band 2 reads as %q", progress.String())
		}
	}

	if _, err := NewSidebandWriter(io.Discard, BandData, 5).Write([]byte("x")); err == nil {
		t.Error("a side-band packet with no room for data is written")
	}
	var big bytes.Buffer
	NewSidebandWriter(&big, BandData, 1<<20).Write(data)
	if _, p, _ := NewPktReader(&big).Next(); len(p)+4 != MaxPktLen {
		t.Errorf("a side-band writer asked for packets past the largest writes one of %d bytes", len(p)+4)
	}

	var b bytes.Buffer
	NewSidebandWriter(&b, BandData, SidebandMaxPkt).Write([]byte("half"))
	NewSidebandWriter(&b, BandError, SidebandMaxPkt).Write([]byte("refused\n"))
	_, err := io.ReadAll(NewSidebandReader(NewPktReader(&b)))
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Message != "refused" {
		t.Errorf("band 3 reads as %v", err)
	}
	for name, in := range map[string]string{
		"a band past 3":                   "0006\x04x0000",
		"a delimiter inside the response": "0001",
		"a response that never ends":      "0006\x01x",
	} {
		if _, err := io.ReadAll(NewSidebandReader(NewPktReader(strings.NewReader(in)))); err == nil {
			t.Errorf("%s reads", name)
		}
	}
}

func TestPacketsReadWhatGitUploadPackWrites(t *testing.T) {
	s := newSample(t)
	refs := s.refs(t)

	// Protocol version 0: the advertisement, HEAD first with the capabilities after a null byte.
	adv := packets(t, git(t, s.dir, nil, "upload-pack", "--stateless-rpc", "--advertise-refs", "."))
	if first, caps, _ := strings.Cut(adv[0], "\x00"); first != gitID(t, s.dir, "HEAD").String()+" HEAD" || !strings.Contains(caps, "symref=HEAD:refs/heads/main") {
		t.Errorf("the advertisement begins %q", adv[0])
	}
	if adv[len(adv)-1] != "[flush]" {
		t.Errorf("the advertisement ends with %q", adv[len(adv)-1])
	}
	advertised := map[string]ID{}
	for _, line := range adv[1 : len(adv)-1] {
		id, ref, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if !strings.HasSuffix(ref, "^{}") {
			advertised[ref], _ = ParseID(id)
		}
	}
	if !maps.Equal(advertised, refs) {
		t.Errorf("the advertisement names %v, and the repository holds %v", advertised, refs)
	}

	// Protocol version 2: ls-refs, its arguments after a delimiter.
	v2 := []string{"GIT_PROTOCOL=version=2"}
	var req bytes.Buffer
	WritePktString(&req, "command=ls-refs\n")
	WritePktString(&req, "object-format=sha1\n")
	WriteDelim(&req)
	WritePktString(&req, "symrefs\n")
	WritePktString(&req, "peel\n")
	WriteFlush(&req)
	out, err := gitErr(s.dir, req.Bytes(), v2, "upload-pack", "--stateless-rpc", ".")
	if err != nil {
		t.Fatal(err)
	}
	listed := packets(t, out)
	if !strings.Contains(strings.Join(listed, ""), "HEAD symref-target:refs/heads/main") {
		t.Errorf("ls-refs answers %q", listed)
	}
	if n := len(listed); n != len(refs)+2 || listed[n-1] != "[flush]" {
		t.Errorf("ls-refs answers %d packets for %d refs and HEAD", n, len(refs))
	}

	// And fetch, whose pack comes on band 1 with git's progress on band 2.
	req.Reset()
	WritePktString(&req, "command=fetch\n")
	WritePktString(&req, "object-format=sha1\n")
	WriteDelim(&req)
	WritePktString(&req, "ofs-delta\n")
	WritePktString(&req, "want "+s.second.String()+"\n")
	WritePktString(&req, "done\n")
	WriteFlush(&req)
	if out, err = gitErr(s.dir, req.Bytes(), v2, "upload-pack", "--stateless-rpc", "."); err != nil {
		t.Fatal(err)
	}
	r := NewPktReader(bytes.NewReader(out))
	if _, section, err := r.Next(); err != nil || string(section) != "packfile\n" {
		t.Fatalf("the fetch answers %q first: %v", section, err)
	}
	var progress bytes.Buffer
	band := NewSidebandReader(r)
	band.Progress = &progress
	pack, err := io.ReadAll(band)
	if err != nil {
		t.Fatal(err)
	}
	var written bytes.Buffer
	u, err := Unpack(context.Background(), bytes.NewReader(pack), int64(len(pack)), &written, UnpackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := s.objects(t, s.second.String()); len(u.Objects) != len(want) {
		t.Errorf("the pack fetched holds %d objects, and git says %s reaches %d", len(u.Objects), s.second, len(want))
	}
	if !strings.Contains(progress.String(), "objects") {
		t.Errorf("band 2 carried %q, and git counts its objects there", progress.String())
	}
}
