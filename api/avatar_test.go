package api_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	pictures "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// What PUT /api/v1/me/avatar stores of a photo, with no database: a PNG of its pixels alone, scaled
// down to 512 a side at most and turned upright, and a refusal for a file that is no photo or that
// announces too many pixels.

// whereabouts is what a phone writes into a photo's Exif beside the picture, which nothing stored
// may carry.
const whereabouts = "GPSLatitude 45.7640N GPSLongitude 4.8357E"

// withExif answers a JPEG with an Exif block written after its first marker, carrying orientation
// and whereabouts, as a phone's camera writes one.
func withExif(t *testing.T, picture []byte, orientation uint16) []byte {
	t.Helper()
	if len(picture) < 2 || picture[0] != 0xFF || picture[1] != 0xD8 {
		t.Fatal("not a JPEG")
	}
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	binary.Write(&tiff, binary.BigEndian, uint16(42))
	binary.Write(&tiff, binary.BigEndian, uint32(8))
	binary.Write(&tiff, binary.BigEndian, uint16(1))      // one entry
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	binary.Write(&tiff, binary.BigEndian, uint16(3))      // SHORT
	binary.Write(&tiff, binary.BigEndian, uint32(1))
	binary.Write(&tiff, binary.BigEndian, orientation)
	binary.Write(&tiff, binary.BigEndian, uint16(0))
	binary.Write(&tiff, binary.BigEndian, uint32(0)) // no directory after it
	tiff.WriteString(whereabouts)
	segment := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(picture[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(segment)+2))
	out.Write(segment)
	out.Write(picture[2:])
	return out.Bytes()
}

// quarters is a picture of w by h whose quarters are red at the top left, blue at the top right,
// green at the bottom left and black at the bottom right, so that which way it was turned shows.
func quarters(w, h int) *pictures.NRGBA {
	picture := pictures.NewNRGBA(pictures.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.NRGBA{A: 255}
			switch {
			case x < w/2 && y < h/2:
				c.R = 255
			case y < h/2:
				c.B = 255
			case x < w/2:
				c.G = 255
			}
			picture.SetNRGBA(x, y, c)
		}
	}
	return picture
}

// aJPEG is picture encoded as a JPEG at the best quality, so that its colours come back near enough.
func aJPEG(t *testing.T, picture pictures.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, picture, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// colourAt names the colour of a stored photo at x, y, among the four quarters paints.
func colourAt(picture pictures.Image, x, y int) string {
	r, g, b, _ := picture.At(x, y).RGBA()
	r, g, b = r>>8, g>>8, b>>8
	switch {
	case r > 200 && g < 60 && b < 60:
		return "red"
	case b > 200 && r < 60 && g < 60:
		return "blue"
	case g > 200 && r < 60 && b < 60:
		return "green"
	case r < 60 && g < 60 && b < 60:
		return "black"
	}
	return "something else"
}

// A JPEG carrying an Exif block, its whereabouts among it, is stored as a PNG of its pixels alone,
// scaled down to 512 on its longer side and keeping its aspect.
func TestAPhotoIsStoredAsAPNGOfItsPixelsAlone(t *testing.T) {
	sent := withExif(t, aJPEG(t, quarters(600, 300)), 1)
	if !bytes.Contains(sent, []byte(whereabouts)) {
		t.Fatal("the photo sent does not carry its whereabouts, so this proves nothing")
	}
	stored, width, height, err := api.Reencode(sent)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(whereabouts)) || bytes.Contains(stored, []byte("Exif")) {
		t.Error("the photo stored carries the Exif of the file sent")
	}
	picture, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("the photo stored is no PNG: %s", err)
	}
	if b := picture.Bounds(); b.Dx() != 512 || b.Dy() != 256 || width != 512 || height != 256 {
		t.Errorf("a photo of 600 by 300 is stored at %d by %d, and answered as %d by %d", b.Dx(), b.Dy(), width, height)
	}
	if got := colourAt(picture, 128, 64) + " " + colourAt(picture, 384, 192); got != "red black" {
		t.Errorf("the photo stored reads %s at its top left and bottom right quarters", got)
	}

	// One that fits is encoded again all the same, at its own size.
	small := quarters(40, 30)
	var sentPNG bytes.Buffer
	png.Encode(&sentPNG, small)
	if _, width, height, err := api.Reencode(sentPNG.Bytes()); err != nil || width != 40 || height != 30 {
		t.Errorf("a PNG of 40 by 30 is stored at %d by %d, %v", width, height, err)
	}
	// A tall one keeps its aspect too.
	if _, width, height, err := api.Reencode(aJPEG(t, quarters(300, 1200))); err != nil || width != 128 || height != 512 {
		t.Errorf("a JPEG of 300 by 1200 is stored at %d by %d, %v", width, height, err)
	}
}

// A JPEG is turned as its Exif orientation says it is seen, each of the eight, so that a photo taken
// with the phone on its side is stored as it was seen and not as the sensor read it.
func TestAPhotoIsStoredUpright(t *testing.T) {
	for _, c := range []struct {
		orientation uint16
		// The colours of the stored photo's quarters: top left, top right, bottom left and
		// bottom right.
		want string
	}{
		{1, "red blue green black"},
		{2, "blue red black green"},
		{3, "black green blue red"},
		{4, "green black red blue"},
		{5, "red green blue black"},
		{6, "green red black blue"},
		{7, "black blue green red"},
		{8, "blue black red green"},
	} {
		stored, width, height, err := api.Reencode(withExif(t, aJPEG(t, quarters(80, 40)), c.orientation))
		if err != nil {
			t.Fatal(err)
		}
		picture, err := png.Decode(bytes.NewReader(stored))
		if err != nil {
			t.Fatal(err)
		}
		if turned := c.orientation >= 5; (turned && (width != 40 || height != 80)) || (!turned && (width != 80 || height != 40)) {
			t.Errorf("orientation %d is stored at %d by %d", c.orientation, width, height)
			continue
		}
		var got []string
		for _, at := range [][2]int{{width / 4, height / 4}, {3 * width / 4, height / 4}, {width / 4, 3 * height / 4}, {3 * width / 4, 3 * height / 4}} {
			got = append(got, colourAt(picture, at[0], at[1]))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("orientation %d is stored as %s, want %s", c.orientation, strings.Join(got, " "), c.want)
		}
	}
}

// pngAnnouncing is the signature of a PNG and a header announcing w by h, which is all a header
// read before decoding finds.
func pngAnnouncing(w, h uint32) []byte {
	var header bytes.Buffer
	header.WriteString("IHDR")
	binary.Write(&header, binary.BigEndian, w)
	binary.Write(&header, binary.BigEndian, h)
	header.Write([]byte{8, 6, 0, 0, 0})
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	binary.Write(&out, binary.BigEndian, uint32(13))
	out.Write(header.Bytes())
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(header.Bytes()))
	return out.Bytes()
}

// A file that is no PNG or JPEG is refused, and so is one whose header announces more than 2048
// pixels a side, before anything is decoded: a few bytes announcing a picture that would take
// gigabytes to hold.
func TestAPhotoThatIsNoneOrTooLargeIsRefused(t *testing.T) {
	var gif bytes.Buffer
	gif.WriteString("GIF89a")
	for _, c := range []struct {
		name string
		raw  []byte
		says string
	}{
		{"text", []byte("not a photo at all"), "not a PNG or a JPEG"},
		{"a GIF", gif.Bytes(), "not a PNG or a JPEG"},
		{"a PNG cut after its header", pngAnnouncing(64, 64), "can be read whole"},
		{"a PNG announcing 4096 by 4096", pngAnnouncing(4096, 4096), "4096 by 4096 pixels"},
		{"a PNG announcing 2049 by 1", pngAnnouncing(2049, 1), "2049 by 1 pixels"},
		{"a PNG announcing 1 by 3000000", pngAnnouncing(1, 3000000), "1 by 3000000 pixels"},
	} {
		if _, _, _, err := api.Reencode(c.raw); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s was answered %v", c.name, err)
		}
	}
}
