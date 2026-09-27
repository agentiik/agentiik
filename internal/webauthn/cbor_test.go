package webauthn

import (
	"bytes"
	"encoding/hex"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func h(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

// What the decoder reads, and what it reads it as.
func TestTheDecoderReadsTheSubsetWebAuthnWrites(t *testing.T) {
	for _, c := range []struct {
		in   string
		want any
	}{
		{"00", int64(0)},
		{"17", int64(23)},
		{"18 18", int64(24)},
		{"18 ff", int64(255)},
		{"19 0100", int64(256)},
		{"1a 00010000", int64(65536)},
		{"1b 0000000100000000", int64(1 << 32)},
		{"1b 7fffffffffffffff", int64(math.MaxInt64)},
		{"20", int64(-1)},
		{"38 18", int64(-25)},
		{"39 0100", int64(-257)},
		{"3b 7fffffffffffffff", int64(math.MinInt64)},
		{"40", []byte{}},
		{"43 010203", []byte{1, 2, 3}},
		{"60", ""},
		{"63 616263", "abc"},
		{"62 c3a9", "é"},
		{"80", []any{}},
		{"82 01 20", []any{int64(1), int64(-1)}},
		{"a0", map[any]any{}},
		{"f4", false},
		{"f5", true},
		// 1 and "1" are two keys.
		{"a2 01 00 61 31 01", map[any]any{int64(1): int64(0), "1": int64(1)}},
		// Keys out of the canonical order are read: once each appears once, order changes nothing.
		{"a2 61 62 01 61 61 02", map[any]any{"b": int64(1), "a": int64(2)}},
		// Five levels of arrays and maps, the deepest there is.
		{"81 a1 00 81 81 81 00", []any{map[any]any{int64(0): []any{[]any{[]any{int64(0)}}}}}},
	} {
		got, err := decodeCBOR(h(c.in))
		if err != nil {
			t.Errorf("%s: %s", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s reads as %#v, where it is %#v", c.in, got, c.want)
		}
	}
}

// What the decoder refuses, and that it says why.
func TestTheDecoderRefusesEverythingElse(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"nothing", "", "ends inside an item"},
		{"a head cut short", "19 01", "ends inside an item"},
		{"a head with nothing after", "18", "ends inside an item"},
		{"a string cut short", "43 0102", "runs past the end"},
		{"a string longer than anything", "5b 7fffffffffffffff", "runs past the end"},
		{"an array longer than the input", "9a ffffffff", "runs past the end"},
		{"an array of more items than bytes left", "82 00", "runs past the end"},
		{"an array whose item is cut short", "82 19 01", "ends inside an item"},
		{"a map of more entries than bytes left for them", "a2 00 00", "runs past the end"},
		{"a map whose key is cut short", "a1 19 01", "ends inside an item"},
		{"a map whose value is missing", "a1 61 61", "ends inside an item"},

		{"an integer on two bytes where one does", "18 17", "shortest form"},
		{"an integer on three bytes where two do", "19 00ff", "shortest form"},
		{"an integer on five bytes where three do", "1a 0000ffff", "shortest form"},
		{"an integer on nine bytes where five do", "1b 00000000ffffffff", "shortest form"},
		{"a negative integer written long", "38 00", "shortest form"},
		{"a string length written long", "58 01 00", "shortest form"},
		{"a text length written long", "78 01 61", "shortest form"},
		{"an array length written long", "98 00", "shortest form"},
		{"a map length written long", "b8 00", "shortest form"},

		{"an indefinite byte string", "5f 41 00 ff", "indefinite length"},
		{"an indefinite text string", "7f 61 61 ff", "indefinite length"},
		{"an indefinite array", "9f 00 ff", "indefinite length"},
		{"an indefinite map", "bf 00 00 ff", "indefinite length"},
		{"an integer with additional information 31", "1f", "indefinite length"},
		{"a break on its own", "ff", "simple value"},
		{"additional information 28", "1c", "reserved"},
		{"additional information 29", "3d", "reserved"},
		{"additional information 30", "5e", "reserved"},

		{"a tag", "c0 00", "a tag is refused"},
		{"a tag of one byte", "d8 18 00", "a tag is refused"},
		{"a half float", "f9 0000", "float"},
		{"a single float", "fa 00000000", "float"},
		{"a double float", "fb 0000000000000000", "float"},
		{"null", "f6", "simple value"},
		{"undefined", "f7", "simple value"},
		{"a simple value", "e0", "simple value"},
		{"a simple value on two bytes", "f8 20", "simple value"},

		{"an integer past 63 bits", "1b 8000000000000000", "does not fit"},
		{"a negative integer past 63 bits", "3b 8000000000000000", "does not fit"},
		{"text that is not UTF-8", "61 ff", "not valid UTF-8"},
		{"a surrogate in text", "63 eda080", "not valid UTF-8"},

		{"an integer key twice", "a2 01 00 01 01", "appears twice"},
		{"a text key twice", "a2 61 61 00 61 61 01", "appears twice"},
		{"a byte string key", "a1 41 00 00", "only integers and text"},
		{"an array key", "a1 80 00", "only integers and text"},
		{"a boolean key", "a1 f5 00", "only integers and text"},

		{"a byte after the item", "00 00", "1 bytes follow the item"},
		{"six levels of arrays", "81 81 81 81 81 81 00", "nest deeper than 5"},
		{"six levels of maps", "a1 00 a1 00 a1 00 a1 00 a1 00 a1 00 00", "nest deeper than 5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := decodeCBOR(h(c.in))
			if err == nil {
				t.Fatalf("%s reads as %#v, where it is refused", c.in, v)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s is refused with %q, where the refusal says %q", c.in, err, c.want)
			}
		})
	}
}

// An input holds 256 items at most, and a head claiming more than what is left of them is refused
// before anything is allocated for it.
func TestTheDecoderRefusesMoreItemsThanWebAuthnWrites(t *testing.T) {
	flat := func(n int) []byte { return append(encHead(majorArray, uint64(n)), make([]byte, n)...) }
	if _, err := decodeCBOR(flat(maxItems - 1)); err != nil {
		t.Fatalf("an array of %d items, %d with itself, gave %v", maxItems-1, maxItems, err)
	}
	for name, in := range map[string][]byte{
		"an array of 256 items":                       flat(maxItems),
		"a map of 128 entries":                        append(encHead(majorMap, maxItems/2), make([]byte, maxItems)...),
		"arrays whose items add up to 257":            append(append(h("82"), flat(200)...), flat(54)...),
		"a last item past the budget":                 append(append(h("82"), flat(maxItems-2)...), 0),
		"a claim past the budget of what is left":     append(h("82 00"), encHead(majorArray, maxItems-2)...),
		"a map claim past the budget of what is left": append(h("82 00"), encHead(majorMap, maxItems/2)...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeCBOR(append(in, make([]byte, 600)...))
			if err == nil || !strings.Contains(err.Error(), "256 items") {
				t.Fatalf("gave %v, where it is refused for holding more than 256 items", err)
			}
		})
	}
}

// What a hostile input makes the decoder allocate stays small: heads claiming thousands of entries,
// nested as deep as allowed and padded to the size limit, cost kilobytes, not the megabytes a
// decoder allocating for every claim would spend before refusing them.
func TestAHostileInputCostsTheDecoderLittle(t *testing.T) {
	for name, head := range map[string][]byte{"maps": encHead(majorMap, 8000), "arrays": encHead(majorArray, 16000)} {
		var in []byte
		for range maxDepth {
			in = append(in, head...)
			in = append(in, 0)
		}
		in = append(in, make([]byte, maxInput-len(in))...)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		const runs = 20
		for range runs {
			if _, err := decodeCBOR(in); err == nil {
				t.Fatalf("the hostile %s were read", name)
			}
		}
		runtime.ReadMemStats(&after)
		if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 32<<10 {
			t.Fatalf("decoding %d bytes of hostile %s allocated %d bytes, where it stays under 32 KiB", len(in), name, per)
		}
	}
}

// An input past the size limit is refused before a byte of it is read.
func TestTheDecoderRefusesAnInputPastItsSize(t *testing.T) {
	big := enc(bytes.Repeat([]byte{1}, maxInput))
	if _, err := decodeCBOR(big); err == nil || !strings.Contains(err.Error(), "more than the") {
		t.Fatalf("an input of %d bytes gave %v", len(big), err)
	}
	fits := enc(bytes.Repeat([]byte{1}, maxInput-3))
	if _, err := decodeCBOR(fits); err != nil {
		t.Fatalf("an input of %d bytes gave %v", len(fits), err)
	}
}

// A prefix read says where the item ends, which is how the authenticator data finds its extensions.
func TestAPrefixReadSaysWhereTheItemEnds(t *testing.T) {
	v, n, err := decodeCBORPrefix(h("a1 01 02 ff ff"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || !reflect.DeepEqual(v, map[any]any{int64(1): int64(2)}) {
		t.Fatalf("read %#v in %d bytes, where it is {1: 2} in 3", v, n)
	}
}

// A byte string read is the decoder's own, so a caller reusing its buffer changes no credential.
func TestAByteStringReadIsACopy(t *testing.T) {
	in := h("42 0102")
	v, err := decodeCBOR(in)
	if err != nil {
		t.Fatal(err)
	}
	in[1] = 9
	if v.([]byte)[0] != 1 {
		t.Fatal("the byte string read changed with the input")
	}
}

// The decoder, fuzzed. Whatever it accepts is written back by the tests' encoder, which writes the
// shortest form, in as many bytes as it came in, and reads back as the same value: a decoder that
// accepted a longer spelling, read past the end or dropped a byte would fail one of the two.
func FuzzDecodeCBOR(f *testing.F) {
	for _, s := range []string{
		"00", "1b 7fffffffffffffff", "3b 7fffffffffffffff", "43 010203", "63 616263", "82 01 20",
		"a2 01 00 61 31 01", "81 a1 00 81 81 00", "f4", "f5", "18 17", "5f 41 00 ff", "a2 01 00 01 01",
		"c0 00", "f9 0000", "9a ffffffff", "81 81 81 81 81 00", "61 ff",
	} {
		f.Add(h(s))
	}
	for _, v := range vectorsForFuzzing(f) {
		f.Add(v.attestationObject)
		f.Add(v.publicKey)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		v, err := decodeCBOR(in)
		if err != nil {
			return
		}
		again := enc(v)
		if len(again) != len(in) {
			t.Fatalf("%x read as %#v, which is %d bytes written in its shortest form, not %d", in, v, len(again), len(in))
		}
		w, err := decodeCBOR(again)
		if err != nil {
			t.Fatalf("%x read as %#v, written back as %x, which does not read: %s", in, v, again, err)
		}
		if !reflect.DeepEqual(v, w) {
			t.Fatalf("%x read as %#v and, written back, as %#v", in, v, w)
		}
	})
}

// fuzzSeed is what the spec vectors give the fuzzers to start from.
type fuzzSeed struct{ attestationObject, authData, publicKey, assertionData []byte }

func vectorsForFuzzing(f *testing.F) []fuzzSeed {
	var seeds []fuzzSeed
	for _, v := range vectors(f) {
		obj := unhex(f, v.AttestationObject)
		decoded, err := decodeCBOR(obj)
		if err != nil {
			f.Fatal(err)
		}
		ad := decoded.(map[any]any)["authData"].([]byte)
		parsed, err := parseAuthenticatorData(ad)
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, fuzzSeed{obj, ad, parsed.attested.publicKey, unhex(f, v.AuthenticatorData)})
	}
	return seeds
}
