package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// An MCP client's Client ID Metadata Document: "MCP clients and authorization servers SHOULD support
// OAuth Client ID Metadata Documents", which the revision prefers to registering clients, since
// "servers and clients have no pre-existing relationship". The client's identifier is the https URL of
// a JSON document it publishes, and the installation fetches it when an authorisation is asked for,
// for the client's name and its redirect URIs, and keeps nothing about the client but the grants its
// people gave it.
//
// The fetch is a request the installation makes to an address anybody may write, so it is held to
// draft-ietf-oauth-client-id-metadata-document-00, section 6, and the revision's Client ID Metadata
// Document Security: no address of the loopback, a private network or any other special-use range,
// checked on the address dialled, since a name may resolve to one; no redirect followed, "MUST NOT
// automatically follow HTTP redirects"; Accept: application/json, so that a server holding something
// else refuses early; and at most documentMost read, "the recommended maximum size to read is 5
// kilobytes". It goes through no proxy, whose own address would be one the check refuses, and which a
// document the installation fetches for itself has no reason to cross.

// The bounds of a fetch.
const (
	documentMost = 5 << 10
	documentWait = 5 * time.Second
)

// How long a document is kept, by each process of the API: as long as its Cache-Control says, since
// the server "SHOULD respect HTTP cache headers", an hour at most, since it "SHOULD enforce a maximum
// effective cache duration", and five minutes where it says nothing, "a minimum effective cache
// duration when none is otherwise indicated". An hour is short enough that a client withdrawing a
// redirect URI is heard within a working session, and five minutes long enough that the page and the
// sign-in it serves read one document.
const (
	documentKeptMost  = time.Hour
	documentKeptLeast = 5 * time.Minute
)

// documentsKept is how many documents one process keeps at once: enough for every client an
// installation's people use, and few enough that a caller writing a fresh client_id at every request
// cannot make the process keep without bound, since a document past it replaces the one kept longest.
const documentsKept = 1000

// fetchesMost is how many documents one process fetches at once: anybody may open the authorisation
// page naming any client_id, and each opening is a request the installation makes for them, so that
// a caller opening it as fast as they can has the installation make at most this many requests at a
// time rather than as many as they send. Enough for the people of an installation allowing their
// clients at once, since a document is kept once fetched; a request past it is refused, and its
// person tries again.
const fetchesMost = 16

// clientNameMost is the longest client_name taken, in characters: a token's device_label, which the
// name of the client its grant was allowed to becomes.
const clientNameMost = deviceLabelMax

// clientDocument is what the installation reads of a client's document.
type clientDocument struct {
	// ID is its client_id, the URL it was fetched from to the character.
	ID string

	// Name is its client_name, one line, which the page shows and the grant is listed under.
	Name string

	// RedirectURIs are its redirect_uris, each https or plain http to the loopback.
	RedirectURIs []string
}

// redirects says whether a redirect URI is one the document names: "the AS MUST ensure that the two
// URIs are equal using simple string comparison", but for "native apps using a localhost URI: in this
// case, the authorization server MUST allow variable port numbers", so a loopback one is compared with
// its port left out.
func (d clientDocument) redirects(asked string) bool {
	portless, loopback := loopbackWithoutPort(asked)
	for _, named := range d.RedirectURIs {
		if named == asked {
			return true
		}
		if loopback {
			if other, ok := loopbackWithoutPort(named); ok && other == portless {
				return true
			}
		}
	}
	return false
}

// loopbackWithoutPort is a loopback redirect URI, http to 127.0.0.1, [::1] or localhost, written with
// its port left out, and false for any other.
func loopbackWithoutPort(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return "", false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
	default:
		return "", false
	}
	u.Host = u.Hostname()
	if strings.Contains(u.Host, ":") {
		u.Host = "[" + u.Host + "]"
	}
	return u.String(), true
}

// redirectHolds says whether a redirect URI is one a document may name: "All redirect URIs MUST be
// either localhost or use HTTPS", absolute, with no user and no fragment, as OAuth 2.1 writes one.
func redirectHolds(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || len(uri) > 2048 || u.User != nil || u.Fragment != "" || strings.Contains(uri, "#") || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	_, loopback := loopbackWithoutPort(uri)
	return loopback
}

// clientDocuments fetches documents and keeps them.
type clientDocuments struct {
	fetch *http.Client
	now   func() time.Time

	mu   sync.Mutex
	kept map[string]keptDocument

	// fetching holds a place for each fetch under way, fetchesMost at most.
	fetching chan struct{}
}

// keptDocument is a document kept, until it lapses.
type keptDocument struct {
	document clientDocument
	until    time.Time
}

func newClientDocuments(fetch *http.Client, now func() time.Time) *clientDocuments {
	return &clientDocuments{fetch: fetch, now: now, kept: map[string]keptDocument{}, fetching: make(chan struct{}, fetchesMost)}
}

// get answers the document of client_id id, kept or fetched now, or why none can be read, a sentence
// the page shows. "The authorization server MUST NOT cache error responses ... MUST NOT cache
// documents which are invalid or malformed": only one that holds is kept.
func (c *clientDocuments) get(ctx context.Context, id string) (clientDocument, error) {
	now := c.now()
	c.mu.Lock()
	k, ok := c.kept[id]
	c.mu.Unlock()
	if ok && now.Before(k.until) {
		return k.document, nil
	}
	d, keep, err := c.fetched(ctx, id)
	if err != nil {
		return clientDocument{}, err
	}
	if keep > 0 {
		c.mu.Lock()
		if len(c.kept) >= documentsKept {
			c.forgetOne(now)
		}
		c.kept[id] = keptDocument{document: d, until: now.Add(keep)}
		c.mu.Unlock()
	}
	return d, nil
}

// forgetOne makes room for one more document: every one lapsed, or where none has, the one that
// lapses first. Called with mu held.
func (c *clientDocuments) forgetOne(now time.Time) {
	first := ""
	for id, k := range c.kept {
		if !now.Before(k.until) {
			delete(c.kept, id)
			continue
		}
		if first == "" || k.until.Before(c.kept[first].until) {
			first = id
		}
	}
	if len(c.kept) >= documentsKept && first != "" {
		delete(c.kept, first)
	}
}

// fetched fetches the document at id and holds it to what the installation reads, and answers how long
// it may be kept, none where its Cache-Control says no-store or no-cache.
func (c *clientDocuments) fetched(ctx context.Context, id string) (clientDocument, time.Duration, error) {
	select {
	case c.fetching <- struct{}{}:
		defer func() { <-c.fetching }()
	default:
		return clientDocument{}, 0, fmt.Errorf("the installation is fetching %d client documents already, the most it fetches at once: try again in a moment", fetchesMost)
	}
	ctx, cancel := context.WithTimeout(ctx, documentWait)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return clientDocument{}, 0, errors.New("the client's metadata document could not be asked for at its client_id")
	}
	r.Header.Set("Accept", "application/json")
	w, err := c.fetch.Do(r)
	if err != nil {
		if errors.Is(err, errSpecialUse) {
			return clientDocument{}, 0, errors.New("the client's metadata document is at an address of a private network, the loopback or another special-use range, which the installation never fetches from")
		}
		return clientDocument{}, 0, errors.New("the client's metadata document could not be fetched from its client_id within 5 seconds")
	}
	defer w.Body.Close()
	if w.StatusCode != http.StatusOK {
		return clientDocument{}, 0, fmt.Errorf("the client's metadata document was answered %d at its client_id, where it is served with 200 and followed through no redirect", w.StatusCode)
	}
	// Its type is not asked: "the response is JSON" is what the draft holds a document to, and the
	// JSON is read below, where anything else is refused. A client serving its document from a host
	// that types every file as text is served as well as any.
	raw, err := io.ReadAll(io.LimitReader(w.Body, documentMost+1))
	switch {
	case err != nil:
		return clientDocument{}, 0, errors.New("the client's metadata document could not be read whole")
	case len(raw) > documentMost:
		return clientDocument{}, 0, fmt.Errorf("the client's metadata document is longer than %d bytes, the most the installation reads of one", documentMost)
	}
	d, err := readDocument(raw, id)
	if err != nil {
		return clientDocument{}, 0, err
	}
	return d, keptFor(w.Header.Get("Cache-Control")), nil
}

// readDocument holds a document to what the revision and the draft ask of one, as the installation
// reads it: "the client_id value in the metadata matches the document URL exactly", "at least the
// following properties: client_id, client_name, redirect_uris", each redirect URI https or the
// loopback, and no client authentication but none, since "the token_endpoint_auth_method property
// MUST NOT include client_secret_post, client_secret_basic, client_secret_jwt", and a key, the one
// other way, is a client authentication the installation does not serve: a client declaring one
// would be a confidential client, which "MUST include client authentication of the registered type",
// so it is refused rather than taken as a public one. A grant type or a response type it declares
// includes OAuth's code flow.
func readDocument(raw []byte, id string) (clientDocument, error) {
	var doc struct {
		ClientID                string   `json:"client_id"`
		ClientName              *string  `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod *string  `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return clientDocument{}, errors.New("the client's metadata document is not a JSON object holding client_id, client_name and redirect_uris")
	}
	switch {
	case doc.ClientID != id:
		return clientDocument{}, errors.New("the client's metadata document names another client_id than the URL it was fetched from, which it names to the character")
	case doc.ClientName == nil || strings.TrimSpace(*doc.ClientName) == "":
		return clientDocument{}, errors.New("the client's metadata document gives no client_name, which the installation shows the person asked to allow it")
	case utf8.RuneCountInString(*doc.ClientName) > clientNameMost || strings.ContainsFunc(*doc.ClientName, unicode.IsControl):
		return clientDocument{}, fmt.Errorf("the client's metadata document gives a client_name of more than %d characters, or of more than one line", clientNameMost)
	case len(doc.RedirectURIs) == 0:
		return clientDocument{}, errors.New("the client's metadata document names no redirect_uris, and a code is handed nowhere else")
	case doc.TokenEndpointAuthMethod != nil && *doc.TokenEndpointAuthMethod != "none":
		return clientDocument{}, errors.New("the client's metadata document asks to authenticate at the token endpoint, and the installation authenticates no client: every client is a public one, its code held by PKCE")
	case doc.GrantTypes != nil && !contains(doc.GrantTypes, "authorization_code"):
		return clientDocument{}, errors.New("the client's metadata document names grant_types without authorization_code, the one way a client is allowed here")
	case doc.ResponseTypes != nil && !contains(doc.ResponseTypes, "code"):
		return clientDocument{}, errors.New("the client's metadata document names response_types without code")
	}
	for _, uri := range doc.RedirectURIs {
		if !redirectHolds(uri) {
			return clientDocument{}, errors.New("the client's metadata document names a redirect URI that is neither https nor http to the loopback, which the revision allows alone")
		}
	}
	return clientDocument{ID: id, Name: *doc.ClientName, RedirectURIs: doc.RedirectURIs}, nil
}

func contains(values []string, v string) bool {
	for _, each := range values {
		if each == v {
			return true
		}
	}
	return false
}

// keptFor is how long a document whose answer carried cacheControl is kept: none for no-store or
// no-cache, its max-age within documentKeptMost, and documentKeptLeast where it gives none.
func keptFor(cacheControl string) time.Duration {
	keep := documentKeptLeast
	for _, directive := range strings.Split(cacheControl, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
		switch strings.ToLower(name) {
		case "no-store", "no-cache":
			return 0
		case "max-age":
			seconds, err := strconv.Atoi(strings.Trim(value, `"`))
			if err != nil || seconds < 0 {
				continue
			}
			keep = time.Duration(seconds) * time.Second
		}
	}
	return min(keep, documentKeptMost)
}

// errSpecialUse is an address the guarded fetch dials nothing at.
var errSpecialUse = errors.New("api: the address is of a special-use range, which a client's document is never fetched from")

// guardedFetch is the client documents are fetched with on an installation: the TLS floor, no proxy,
// no redirect followed, the whole of it within documentWait, and a dialer that refuses a special-use
// address once the name is resolved, so that a name resolving to one, or to one the next time it is
// asked, reaches nothing inside.
func guardedFetch() *http.Client {
	dialer := &net.Dialer{Timeout: documentWait, Control: refuseSpecialUse}
	t := tlsfloor.Transport()
	t.Proxy = nil
	t.DialContext = dialer.DialContext
	t.MaxResponseHeaderBytes = 16 << 10
	t.ResponseHeaderTimeout = documentWait
	return &http.Client{
		Transport:     t,
		Timeout:       documentWait,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// refuseSpecialUse is the dialer's check, run on the address it is about to connect to.
func refuseSpecialUse(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || specialUse(ip) {
		return errSpecialUse
	}
	return nil
}

// specialRanges are RFC 6890's special-purpose ranges and those IANA has added since, which "MUST NOT"
// be fetched from: every one but the global unicast space proper.
var specialRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// specialUse says whether an address is in a special-use range, an IPv4 address written as IPv6 read
// as the IPv4 address it is.
func specialUse(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	for _, p := range specialRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
