package webauthn

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// The type of the client data of each ceremony, §5.8.1. It is what keeps a signature made to sign
// in from being presented as a registration, or the other way round.
const (
	typeCreate = "webauthn.create"
	typeGet    = "webauthn.get"
)

// clientData is what a Relying Party checks of the client data, CollectedClientData (§5.8.1). Any
// other member is ignored, as §5.8.1 requires: browsers add some, Chrome sometimes one whose name
// says not to compare the JSON with a template.
type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`

	// Kept raw, so that a member present with any value, null included, is told apart from one
	// that is absent.
	CrossOrigin jsontext.Value `json:"crossOrigin"`
	TopOrigin   jsontext.Value `json:"topOrigin"`
}

// byteOrderMark is stripped from the front of the client data before it is parsed, as the UTF-8
// decode of §7.1 step 5 and §7.2 step 8 does.
var byteOrderMark = []byte("\xef\xbb\xbf")

// checkClientData runs §7.1 steps 5 to 11, or §7.2 steps 8 to 14, on the client data of a ceremony
// of the given type.
//
// It is parsed with encoding/json/v2, which refuses a member written twice and invalid UTF-8, and
// matches member names exactly, where encoding/json keeps the last of two "challenge" members and
// reads "Challenge" as one. A browser writes neither, so client data that has either was not
// written by one, and which of its readings is checked is not a question worth leaving open.
func checkClientData(raw []byte, typ string, c Ceremony) error {
	if len(raw) > maxInput {
		return fmt.Errorf("webauthn: the client data is %d bytes, more than the %d read", len(raw), maxInput)
	}
	var cd clientData
	if err := json.Unmarshal(bytes.TrimPrefix(raw, byteOrderMark), &cd); err != nil {
		return fmt.Errorf("webauthn: the client data is not the JSON object a browser writes: %w", err)
	}
	if cd.Type != typ {
		return fmt.Errorf("webauthn: the client data is of type %q, where this ceremony is %q", cd.Type, typ)
	}
	// The challenge is compared as §7.1 step 8 says, with the base64url encoding of the one issued:
	// with no padding (§3), so a padded or standard encoding of the same bytes is another string.
	if cd.Challenge != base64.RawURLEncoding.EncodeToString(c.Challenge) {
		return fmt.Errorf("webauthn: the client data answers another challenge than the one this ceremony issued")
	}
	if cd.Origin != c.Origin {
		return fmt.Errorf("webauthn: the client data comes from the origin %q, where this installation signs in on %q", cd.Origin, c.Origin)
	}
	// The sign-in page is a top-level page on the installation's own origin, and is never framed
	// by another. A ceremony that says it ran in a cross-origin frame (§7.1 steps 10 and 11) ran
	// somewhere this installation did not put it, and the Relying Party expects no such frame.
	switch string(cd.CrossOrigin) {
	case "", "false":
	case "true":
		return fmt.Errorf("webauthn: the client data says the ceremony ran in a cross-origin frame, and the sign-in page is never framed")
	default:
		return fmt.Errorf("webauthn: crossOrigin in the client data is %s, where it is false or absent", cd.CrossOrigin)
	}
	if cd.TopOrigin != nil {
		return fmt.Errorf("webauthn: the client data names a top-level origin, so the ceremony ran in a frame, and the sign-in page is never framed")
	}
	return nil
}
