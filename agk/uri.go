package agk

import (
	"encoding/json"
	"fmt"
	"strings"
)

// uriPrefix is the whole of the scheme and the one host the form uses. The host is
// literally run, so an artifact URI says what it addresses before it says which one.
const uriPrefix = "agk://run/"

// uriForm is the shape, written out for the errors that refuse a departure from it.
const uriForm = "agk://run/<run>/<step>/<port>/<name>"

// URI is where an artifact is addressed from: the logical name of one artifact on one
// port of one step of one run.
//
// It stays logical. The physical key it resolves to is sha256/<digest>, held per
// namespace by the artifact store, and the URI itself is never handed to a consuming
// container: a brick receives a mounted path and cannot reach the store at all.
type URI struct {
	Run  RunID  `json:"-"`
	Step Step   `json:"-"`
	Port Port   `json:"-"`
	Name string `json:"-"`
}

// String writes the logical form.
func (u URI) String() string {
	return uriPrefix + string(u.Run) + "/" + string(u.Step) + "/" + string(u.Port) + "/" + u.Name
}

// MarshalJSON writes the URI as the single string the envelope carries.
func (u URI) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

// UnmarshalJSON reads one and refuses anything that is not the logical form. An
// envelope naming an artifact by an http URL or by a store key is refused here rather
// than being resolved: the URI is the only way an artifact is addressed.
func (u *URI) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return reject("", "an artifact URI is a string, written as %s", uriForm)
	}
	p, err := ParseURI(s)
	if err != nil {
		return reject("", "%s", err)
	}
	*u = p
	return nil
}

// ParseURI reads the logical form.
func ParseURI(s string) (URI, error) {
	rest, ok := strings.CutPrefix(s, uriPrefix)
	if !ok {
		return URI{}, fmt.Errorf("%q is not an artifact URI: one is written %s", s, uriForm)
	}
	p := strings.Split(rest, "/")
	if len(p) != 4 {
		return URI{}, fmt.Errorf("%q is not an artifact URI: one is written %s, and names a run, a step, a port and a name", s, uriForm)
	}
	u := URI{Run: RunID(p[0]), Step: Step(p[1]), Port: Port(p[2]), Name: p[3]}
	if err := u.validate(); err != nil {
		return URI{}, fmt.Errorf("%q is not an artifact URI: %w", s, err)
	}
	return u, nil
}

// validate applies to each segment the rule that segment carries elsewhere, so that a
// URI that parses is a URI that can be built into a path and a key.
func (u URI) validate() error {
	if err := u.Run.Validate(); err != nil {
		return fmt.Errorf("its run is not one: %w", err)
	}
	if err := u.Step.Validate(); err != nil {
		return fmt.Errorf("its step is not one: %w", err)
	}
	if err := u.Port.Validate(); err != nil {
		return fmt.Errorf("its port is not one: %w", err)
	}
	if err := segment(u.Name); err != nil {
		return fmt.Errorf("its name is not one: %w", err)
	}
	return nil
}
