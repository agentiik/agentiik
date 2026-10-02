package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
)

// A user's photo: PUT, GET and DELETE /api/v1/me/avatar, the caller's own, and GET and DELETE
// /api/v1/users/{login}/avatar, an administrator's. A namespace's picture, at
// /api/v1/namespaces/{ns}/avatar, is "held to a user's photo's rules", and is read, encoded and
// served by the same code: see NamespaceAPI.
//
// A photo is shown to its owner and to administrators alone. Which users an installation has is not
// something another user may ask it, as GET /api/v1/users is an administrator's, and a photo read by
// login would answer that question, a 404 or a picture, for every login somebody cares to try.
//
// What is stored is never what was sent. The file is decoded and encoded again as a PNG, at most
// 512 by 512, so that nothing of the file but its pixels is kept: no Exif block, and no location
// a phone wrote into it, which a photo shown to colleagues would otherwise hand them. Its size in
// pixels is read from its header first, and a file announcing more than 2048 by 2048 is refused
// before a pixel of it is decoded, since a few kilobytes of PNG can announce a picture that takes
// gigabytes to hold.

// avatarMaxBytes is the most a photo sent may weigh: a mebibyte, room for a photo of 2048 by 2048
// compressed as a JPEG, and a bound on what a request makes the API hold before it decodes anything.
const avatarMaxBytes = 1 << 20

// avatarMaxSide is the most pixels a side of a photo sent may have, read from its header before it
// is decoded: 2048, four times the side it is stored at, so that decoding one holds at most 32 MiB
// of pixels, sixteen bits a channel, whatever its file weighs.
const avatarMaxSide = 2048

// avatarSide is the most pixels a side of a stored photo has: 512, sharp wherever a profile
// picture is drawn, on a screen of two or three pixels to the point among them, and small enough
// that every photo of an installation fits in its database without anybody counting them.
const avatarSide = 512

// The absences a photo is answered with, each a 404.
const (
	noOwnAvatar  = "you hold no photo"
	noUserAvatar = "no user by that login, or they hold no photo"
)

// avatar is GET /api/v1/me/avatar: the caller's photo. The bootstrap token and a service account
// hold none, and a token narrowed by a scope reads its holder's as GET /api/v1/me reads who they
// are.
func (m *MeAPI) avatar(w http.ResponseWriter, r *http.Request, caller Caller) {
	if caller.Principal == BootstrapOperator || strings.Contains(string(caller.Principal), "/") {
		fail(w, http.StatusNotFound, noOwnAvatar)
		return
	}
	serveAvatar(w, r, m.pool, string(caller.Principal), noOwnAvatar)
}

// avatar is GET /api/v1/users/{login}/avatar: a user's photo, read by an administrator, as the user
// record is. No user and no photo are one 404.
func (s *UserAPI) avatar(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	login := r.PathValue("login")
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUserAvatar)
		return
	}
	serveAvatar(w, r, s.pool, login, noUserAvatar)
}

// serveAvatar answers login's photo, or absent with 404 where there is none.
func serveAvatar(w http.ResponseWriter, r *http.Request, pool *db.Pool, login, absent string) {
	var picture []byte
	var at time.Time
	err := pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		picture, at, err = wide.Avatar(ctx, login)
		return err
	})
	switch {
	case errors.Is(err, db.ErrNoAvatar), errors.Is(err, db.ErrNoPrincipal):
		fail(w, http.StatusNotFound, absent)
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "the photo could not be read")
		return
	}
	servePicture(w, r, picture, at)
}

// servePicture answers a picture as it is stored, a user's photo or a namespace's picture, set at at.
//
// Cached a day, privately, since the picture is for those who may read its record and no shared
// cache may hand it to anybody else: the console asks for it with ?v= and the record's
// avatar_updated_at, so that a picture set again is asked for at another address. Its tag is that
// instant, which a client keeping the picture past the day revalidates with and is answered 304
// while it is the same. nosniff, so that a browser shows it as the PNG it is declared as and never as
// anything its bytes might be taken for.
func servePicture(w http.ResponseWriter, r *http.Request, picture []byte, at time.Time) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", `"`+strconv.FormatInt(at.UnixMicro(), 10)+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(picture))
}

// setAvatar is PUT /api/v1/me/avatar: the caller's photo, a PNG or a JPEG as the body, read as
// pictureSent reads a picture and stored as reencode makes it in place of any before it, and recorded
// as user.avatar with the size it was stored at.
func (m *MeAPI) setAvatar(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := userWriting(w, caller)
	if !ok {
		return
	}
	stored, width, height, ok := pictureSent(w, r, "photo")
	if !ok {
		return
	}
	// To the microsecond the database keeps, so that the instant answered is the one stored.
	at := m.now().Truncate(time.Microsecond)
	err := m.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		err := wide.SetAvatar(ctx, login, stored, at)
		if errors.Is(err, db.ErrNoPrincipal) {
			return errGone
		}
		if err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: login, Action: audit.UserAvatar, Target: login, Result: audit.Done,
			Detail: map[string]any{"removed": false, "width": width, "height": height},
		})
	})
	switch {
	case errors.Is(err, errGone):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noToken)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the photo could not be stored")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// removeAvatar is DELETE /api/v1/me/avatar: the caller's photo removed, which the console draws as
// their initials from then on. Removing none is the same answer, and recorded as unchanged.
func (m *MeAPI) removeAvatar(w http.ResponseWriter, r *http.Request, caller Caller) {
	login, ok := userWriting(w, caller)
	if !ok {
		return
	}
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	err := m.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		held, err := wide.RemoveAvatar(ctx, login)
		if errors.Is(err, db.ErrNoPrincipal) {
			return errGone
		}
		if err != nil {
			return err
		}
		result := audit.Done
		if !held {
			result = audit.Unchanged
		}
		return wide.Audit(ctx, audit.Record{
			Actor: login, Action: audit.UserAvatar, Target: login, Result: result,
			Detail: map[string]any{"removed": true},
		})
	})
	switch {
	case errors.Is(err, errGone):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, noToken)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the photo could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// removeAvatar is DELETE /api/v1/users/{login}/avatar: a user's photo removed by an administrator,
// one that should not be shown to the people the user works with, recorded as user.avatar by the
// administrator. No user and no photo are one 404, as the photo is read.
func (s *UserAPI) removeAvatar(w http.ResponseWriter, r *http.Request, who Principal, _ Target) {
	if err := readIfAny(r, nothingAsked{}, smallMaxBytes); err != nil {
		fail(w, statusOf(err), err.Error())
		return
	}
	login := r.PathValue("login")
	if LoginRef(login) != nil {
		fail(w, http.StatusNotFound, noUserAvatar)
		return
	}
	err := s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		held, err := wide.RemoveAvatar(ctx, login)
		if err != nil {
			return err
		}
		if !held {
			return db.ErrNoAvatar
		}
		if err := stillBootstrapping(ctx, wide, who); err != nil {
			return err
		}
		return wide.Audit(ctx, audit.Record{
			Actor: string(who), Action: audit.UserAvatar, Target: login, Result: audit.Done,
			Detail: map[string]any{"removed": true},
		})
	})
	switch {
	case errors.Is(err, db.ErrNoPrincipal), errors.Is(err, db.ErrNoAvatar):
		fail(w, http.StatusNotFound, noUserAvatar)
	case errors.Is(err, db.ErrBootstrapEnded):
		bootstrapEnded(w)
	case err != nil:
		fail(w, http.StatusInternalServerError, "the photo could not be removed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// pictureSent reads the picture a request sends, a user's photo or a namespace's picture, which noun
// names in what it refuses, and answers it as reencode stores it, with its size in pixels; or answers
// the request with why it was refused, and false.
//
// The Content-Type is judged before a byte is read, and only says the body is one of the two: the
// bytes say which, since a browser names a file's type by its extension, and they are decoded and
// encoded again whatever they claim.
func pictureSent(w http.ResponseWriter, r *http.Request, noun string) ([]byte, int, int, bool) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || (media != "image/png" && media != "image/jpeg") {
		fail(w, http.StatusUnsupportedMediaType, fmt.Sprintf("a %s is sent as image/png or image/jpeg, and this request's Content-Type is %.64q", noun, r.Header.Get("Content-Type")))
		return nil, 0, 0, false
	}
	raw, err := slurp(r, avatarMaxBytes)
	switch {
	case errors.As(err, new(*tooLarge)):
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a %s is at most %d bytes, a mebibyte, and this one is more: send it smaller, since it is stored at %d by %d pixels at most", noun, avatarMaxBytes, avatarSide, avatarSide))
		return nil, 0, 0, false
	case err != nil:
		fail(w, statusOf(err), err.Error())
		return nil, 0, 0, false
	case len(raw) == 0:
		fail(w, http.StatusBadRequest, fmt.Sprintf("the request body is empty, and this route reads a %s, a PNG or a JPEG", noun))
		return nil, 0, 0, false
	}
	stored, width, height, err := reencode(raw, noun)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return nil, 0, 0, false
	}
	return stored, width, height, true
}

// reencode reads a picture sent, a PNG or a JPEG, and answers it encoded again as a PNG of at most
// avatarSide by avatarSide, keeping its aspect, upright, with its size in pixels. It refuses, saying
// why in the words of noun, photo or picture, a body that is neither, and one whose header announces
// more than avatarMaxSide pixels a side, which is refused before it is decoded.
func reencode(raw []byte, noun string) (encoded []byte, width, height int, err error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, 0, 0, fmt.Errorf("the body is not a PNG or a JPEG that can be read, and a %s is one of the two", noun)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > avatarMaxSide || config.Height > avatarMaxSide {
		return nil, 0, 0, fmt.Errorf("the %s is %d by %d pixels, and one is at most %d by %d: it is stored at %d by %d at most, and a larger one takes more to decode than it gives", noun, config.Width, config.Height, avatarMaxSide, avatarMaxSide, avatarSide, avatarSide)
	}
	var decoded image.Image
	orientation := 1
	if format == "png" {
		decoded, err = png.Decode(bytes.NewReader(raw))
	} else {
		decoded, err = jpeg.Decode(bytes.NewReader(raw))
		orientation = exifOrientation(raw)
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("the body is not a PNG or a JPEG that can be read whole, and a %s is one of the two", noun)
	}
	pixels := image.NewRGBA(image.Rect(0, 0, decoded.Bounds().Dx(), decoded.Bounds().Dy()))
	draw.Draw(pixels, pixels.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	upright := orient(shrink(pixels, avatarSide), orientation)
	var out bytes.Buffer
	// Compressed as far as PNG goes, since a picture is encoded once and read at every page that
	// shows it.
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, upright); err != nil {
		return nil, 0, 0, fmt.Errorf("the %s could not be encoded again: %w", noun, err)
	}
	return out.Bytes(), upright.Bounds().Dx(), upright.Bounds().Dy(), nil
}

// shrink answers pixels scaled down to at most side by side, keeping its aspect, and pixels itself
// where it fits already. Each pixel is the average of the box of pixels it stands for, taken with
// their alpha premultiplied, as image.RGBA holds them, so that a transparent pixel's colour lends
// nothing to its neighbours: a box filter, which is what scaling down by a large factor needs and
// what the standard library does not carry.
func shrink(pixels *image.RGBA, side int) *image.RGBA {
	sw, sh := pixels.Bounds().Dx(), pixels.Bounds().Dy()
	if sw <= side && sh <= side {
		return pixels
	}
	dw, dh := side, side
	if sw >= sh {
		dh = max(1, (sh*side+sw/2)/sw)
	} else {
		dw = max(1, (sw*side+sh/2)/sh)
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := range dh {
		// Each box is at least one pixel across, since the picture is scaled down on both sides.
		y0, y1 := dy*sh/dh, (dy+1)*sh/dh
		for dx := range dw {
			x0, x1 := dx*sw/dw, (dx+1)*sw/dw
			var sum [4]int
			for y := y0; y < y1; y++ {
				row := pixels.Pix[y*pixels.Stride:]
				for x := x0; x < x1; x++ {
					for c := range 4 {
						sum[c] += int(row[x*4+c])
					}
				}
			}
			n := (y1 - y0) * (x1 - x0)
			at := dy*out.Stride + dx*4
			for c := range 4 {
				out.Pix[at+c] = uint8((sum[c] + n/2) / n)
			}
		}
	}
	return out
}

// orient answers pixels turned as a JPEG's Exif orientation, 1 to 8, says the picture is seen: a
// phone stores the picture as its sensor read it and writes which way up it was held beside it, and
// the photo stored keeps no Exif to say so, so it is turned here, as a browser shows the file sent.
// 1, and anything outside 1 to 8, is the picture as it is.
func orient(pixels *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return pixels
	}
	w, h := pixels.Bounds().Dx(), pixels.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		// Turned a quarter, so its sides swap.
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := range dh {
		for dx := range dw {
			var sx, sy int
			switch orientation {
			case 2: // mirrored
				sx, sy = w-1-dx, dy
			case 3: // upside down
				sx, sy = w-1-dx, h-1-dy
			case 4: // mirrored and upside down
				sx, sy = dx, h-1-dy
			case 5: // mirrored across the diagonal from the top left
				sx, sy = dy, dx
			case 6: // turned a quarter clockwise to be seen
				sx, sy = dy, h-1-dx
			case 7: // mirrored across the diagonal from the top right
				sx, sy = w-1-dy, h-1-dx
			case 8: // turned a quarter anticlockwise to be seen
				sx, sy = w-1-dy, dx
			}
			copy(out.Pix[dy*out.Stride+dx*4:][:4], pixels.Pix[sy*pixels.Stride+sx*4:][:4])
		}
	}
	return out
}

// exifOrientation reads the Orientation of a JPEG's Exif block, 1 to 8, and answers 1, the picture
// as it is stored, where the file carries none or one that cannot be read: a photo shown on its side
// is a smaller fault than one refused for a block nobody needs to read to decode it.
func exifOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return 1
	}
	at := 2
	for at+4 <= len(raw) {
		if raw[at] != 0xFF {
			return 1
		}
		marker := raw[at+1]
		switch {
		case marker == 0xFF:
			// A fill byte before a marker.
			at++
			continue
		case marker == 0xDA || marker == 0xD9:
			// The picture begins, or the file ends, and the Exif block is written before both.
			return 1
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			// A marker standing alone, with no length after it.
			at += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(raw[at+2:]))
		if length < 2 || at+2+length > len(raw) {
			return 1
		}
		segment := raw[at+4 : at+2+length]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return tiffOrientation(segment[6:])
		}
		at += 2 + length
	}
	return 1
}

// tiffOrientation reads tag 0x0112, Orientation, from the first directory of the TIFF structure an
// Exif block holds, and answers 1 where it is not there or not one short integer from 1 to 8.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:]) != 42 {
		return 1
	}
	first := order.Uint32(tiff[4:])
	if first < 8 || uint64(first)+2 > uint64(len(tiff)) {
		return 1
	}
	directory := int(first)
	entries := int(order.Uint16(tiff[directory:]))
	for i := range entries {
		entry := directory + 2 + 12*i
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:]) != 0x0112 {
			continue
		}
		// A SHORT, type 3, and one of it, whose value sits in the first two bytes of the four.
		if order.Uint16(tiff[entry+2:]) != 3 || order.Uint32(tiff[entry+4:]) != 1 {
			return 1
		}
		if o := int(order.Uint16(tiff[entry+8:])); o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}
