package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/token"
)

// The installation's own OAuth 2.1 authorisation server, for /mcp and each collection: "Agentiik
// issues its own tokens", on its own sign-in, its accounts staying local and no external identity
// provider involved.
//
// The flow is the one MCP revision 2026-07-28 specifies (basic/authorization), and this file is what
// it asks of a server that is both "the resource server and its own authorization server": the
// metadata documents, "MCP servers MUST implement OAuth 2.0 Protected Resource Metadata" and "MCP
// authorization servers MUST provide at least one" of RFC 8414 and OpenID Connect Discovery; the
// authorisation request, held to OAuth 2.1 and PKCE's S256 alone, whose consent is a sign-in on the
// sign-in page (signin.go); the code that sign-in mints (handOff), kept beside agk login's; and the
// challenges a resource answers with. The token endpoint is oauth_token.go's and the clients'
// documents oauth_client.go's.
//
// Each MCP surface is a protected resource, "the canonical URI of the MCP server", which is its URL
// under the public URL: /mcp, and /mcp/collections/{id} for each collection. A token is issued for
// one of them, its audience, and Principals accepts it there alone (servedResource). Nothing is
// configured: "the issuer and the audience are the installation's own, from its public URL", and the
// tokens are opaque, kept as their SHA-256 as an API token is, so that no key set exists to keep,
// publish or rotate.

// The well-known suffixes, RFC 9728 section 3 and RFC 8414 section 3, each inserted "between the host
// component and the path component, if any" of what it describes.
const (
	resourceWellKnown = "/.well-known/oauth-protected-resource"
	serverWellKnown   = "/.well-known/oauth-authorization-server"
)

// The MCP surfaces, as paths under the public URL.
const (
	mcpPath         = "/mcp"
	collectionsPath = "/mcp/collections/"
)

// The endpoints, as paths under the public URL: the authorisation endpoint is a page of the sign-in
// page's, under /auth, and the token endpoint a route of the API's, under the auth word reserved
// since v0.2.
const (
	authorizePath = "/auth/authorize"
	tokenPath     = "/api/v1/auth/oauth/token"
)

// accessTokenLife is how long an access token is good for: "authorization servers SHOULD issue
// short-lived access tokens to reduce the impact of leaked tokens", and an hour is the lifetime OAuth
// 2.1's own example answers, long enough that a client refreshes a few times in a working day.
const accessTokenLife = time.Hour

// grantDays is how long a grant lasts from the sign-in that allowed it, refreshed or not: an API
// token's default, "so a token forgotten in a script stops on its own within a season", and a client
// is allowed again with a sign-in of the day.
const grantDays = tokenDefaultDays

// stateMost is the longest state a client may write, in characters: far more than any client's
// random value, and few enough that the address the browser is sent back to stays one a browser
// keeps.
const stateMost = 1024

// mcpScopes are the scopes /mcp uses, the nine permissions, and collectionScopes a collection's:
// calling one of its tools takes workflow:run, and nothing else is done there.
var (
	mcpScopes        = permissionNames(access.Permissions)
	collectionScopes = []string{string(WorkflowRun)}
)

func permissionNames(ps []Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

// OAuthOptions are what the authorisation server is given.
type OAuthOptions struct {
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: the issuer, and what every
	// resource's URL is under.
	PublicURL string

	// SignIns is what the sign-in routes share: where a request comes from, and the bound on the
	// entries failures append to the audit log, which a refused code is held to as a refused
	// exchange is. One of its own where it is nil.
	SignIns *SignIns

	// Fetch is what the clients' documents are fetched with, and nil the guarded fetch an
	// installation uses (oauth_client.go). A test gives one reaching a server of its own on the
	// loopback, which the guarded fetch refuses.
	Fetch *http.Client

	// Now is the clock codes, tokens and documents lapse by, the wall clock where it is nil.
	Now func() time.Time

	// Trouble is told what could not be done beside the answer, a refusal that could not be
	// recorded, and nothing where it is nil.
	Trouble func(error)
}

// OAuth is the authorisation server.
type OAuth struct {
	pool *db.Pool

	// issuer is the public URL, origin its scheme and host, and path its path, empty where it has
	// none, which every well-known address carries after its suffix.
	issuer, origin, path string

	// ipAddressed is an installation whose public URL names an IP address, where the policy is
	// applied with passwords allowed and no passkey required, as the sign-in applied it.
	ipAddressed bool

	signIns *SignIns
	clients *clientDocuments
	now     func() time.Time
	trouble func(error)
}

// NewOAuth registers the metadata documents and the token endpoint on a router. The authorisation
// endpoint is a page of the sign-in page's, registered by NewSignIn when it is given this server,
// and the sign-in routes mint the code where they are given it too.
func NewOAuth(rt *Router, o OAuthOptions) (*OAuth, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Pool == nil:
		return nil, errors.New("api: no database, and the codes and the grants are kept there")
	}
	origin, err := originOf(o.PublicURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil {
		return nil, err
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.ContainsAny(path, "{}") {
		// A pattern of the router's would read either as a wildcard, and no public URL needs one.
		return nil, errors.New("api: the public URL's path holds a brace, which the authorisation server's addresses cannot carry")
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.SignIns == nil {
		o.SignIns = NewSignIns(false)
	}
	fetch := o.Fetch
	if fetch == nil {
		fetch = guardedFetch()
	}
	s := &OAuth{
		pool: o.Pool, issuer: strings.TrimRight(o.PublicURL, "/"), origin: origin, path: path,
		ipAddressed: net.ParseIP(u.Hostname()) != nil, signIns: o.SignIns,
		clients: newClientDocuments(fetch, o.Now), now: o.Now, trouble: o.Trouble,
	}
	metadata := Public{Why: "the authorisation server's metadata and each resource's are what a client reads before it holds any credential, RFC 8414 and RFC 9728 documents saying where to ask and what for, the same to everybody and changing nothing"}
	for _, m := range []struct {
		pattern string
		handler Handler
	}{
		{serverWellKnown + path, s.serverMetadata},
		{resourceWellKnown + path + mcpPath, s.resourceMetadata},
		{resourceWellKnown + path + collectionsPath + "{id}", s.resourceMetadata},
	} {
		if err := rt.Handle("GET", m.pattern, metadata, m.handler); err != nil {
			return nil, err
		}
	}
	endpoint := Public{Why: "an MCP client trades a code or a refresh token for an access token here, before it holds any: the request is authenticated by the code and the verifier it carries, which nobody but the client holds together, or by the refresh token its grant was issued"}
	if err := rt.Handle("POST", tokenPath, endpoint, s.token); err != nil {
		return nil, err
	}
	return s, nil
}

// serverMetadata is RFC 8414's document: where to ask and what this server does. "If
// code_challenge_methods_supported is absent, the authorization server does not support PKCE and MCP
// clients MUST refuse to proceed", "authorization servers that include the iss parameter MUST
// advertise this", and a server supporting Client ID Metadata Documents "MUST include"
// client_id_metadata_document_supported. No registration_endpoint, since the installation registers
// no client: Dynamic Client Registration "is deprecated" in the revision, kept "for backwards
// compatibility with authorization servers that do not support Client ID Metadata Documents".
func (s *OAuth) serverMetadata(w http.ResponseWriter, _ *http.Request, _ Principal, _ Target) {
	write(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + authorizePath,
		"token_endpoint":                                 s.issuer + tokenPath,
		"scopes_supported":                               mcpScopes,
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
		"client_id_metadata_document_supported":          true,
	})
}

// resourceMetadata is RFC 9728's document for /mcp or a collection: "the resource value returned MUST
// be identical to the URL that the client used to make the request", and "the Protected Resource
// Metadata document returned by the MCP server MUST include the authorization_servers field". A
// collection's is answered for any identifier written as one, whether a collection holds it or not,
// so that the metadata tells nobody which exist.
func (s *OAuth) resourceMetadata(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	resource, scopes := s.issuer+mcpPath, mcpScopes
	if id := r.PathValue("id"); id != "" {
		if !ulidForm.MatchString(id) {
			fail(w, http.StatusNotFound, "no such thing, or not yours")
			return
		}
		resource, scopes = s.issuer+collectionsPath+id, collectionScopes
	}
	write(w, http.StatusOK, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{s.issuer},
		"scopes_supported":         scopes,
		"bearer_methods_supported": []string{"header"},
	})
}

// resourceKey is where a request the MCP server makes of the API, for a tool, carries the resource the
// request it was made for was to, so that the token it carries is held to that one.
type resourceKey struct{}

// forResource is ctx carrying the resource a request the MCP server makes is on behalf of.
func forResource(ctx context.Context, resource string) context.Context {
	return context.WithValue(ctx, resourceKey{}, resource)
}

// servedResource is the resource a request is to, which an OAuth token's audience is held to: the one
// a request the MCP server made carries, and otherwise the URL under base of /mcp or of a collection
// where the request is to one. Any other request is to no resource, and an OAuth token opens nothing
// there, /api/v1 among them: "MCP servers MUST only accept tokens that are valid for use with their
// own resources." Empty where base is, an installation serving no OAuth.
func servedResource(r *http.Request, base string) string {
	if resource, ok := r.Context().Value(resourceKey{}).(string); ok {
		return resource
	}
	if base == "" {
		return ""
	}
	p := r.URL.Path
	if p == mcpPath || (strings.HasPrefix(p, collectionsPath) && ulidForm.MatchString(strings.TrimPrefix(p, collectionsPath))) {
		return base + p
	}
	return ""
}

// metadataOf is the address of the Protected Resource Metadata of the resource at path under the
// public URL: the suffix "inserted between the host component and the path", the public URL's path
// following it where it has one.
func (s *OAuth) metadataOf(path string) string {
	return s.origin + resourceWellKnown + s.path + path
}

// Challenge is the WWW-Authenticate a resource at path answers with: RFC 9728's resource_metadata,
// which "MCP servers MUST" give by this header or the well-known address and give here by both; the
// scopes the resource uses, which "MCP servers SHOULD include", or those a call needs where error is
// insufficient_scope; and error itself, invalid_token where a token came and opened nothing, and none
// where none came, since RFC 6750 has a resource server that was sent no credential say no error.
func (s *OAuth) Challenge(path, err string, scopes []string) string {
	if scopes == nil {
		scopes = mcpScopes
		if path != mcpPath {
			scopes = collectionScopes
		}
	}
	parts := []string{}
	if err != "" {
		parts = append(parts, `error="`+err+`"`)
	}
	parts = append(parts, `scope="`+strings.Join(scopes, " ")+`"`, `resource_metadata="`+s.metadataOf(path)+`"`)
	return "Bearer " + strings.Join(parts, ", ")
}

// OAuthSignIn is openapi.json's oauthAuthorization: what an MCP client opened /auth/authorize with,
// as the page hands it on with the sign-in that allows it, held to its rules again there, since the
// page is anybody's to call. response_type and code_challenge_method are not carried: the page is
// served only for code and S256, the one of each there is.
type OAuthSignIn struct {
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
	Resource      string `json:"resource"`
	Scope         string `json:"scope,omitempty"`
	State         string `json:"state,omitempty"`
}

func (a *OAuthSignIn) field(b *body, name string) error {
	switch name {
	case "client_id":
		return text(b, &a.ClientID)
	case "redirect_uri":
		return text(b, &a.RedirectURI)
	case "code_challenge":
		return text(b, &a.CodeChallenge)
	case "resource":
		return text(b, &a.Resource)
	case "scope":
		return text(b, &a.Scope)
	case "state":
		return text(b, &a.State)
	}
	return unknown(name)
}

// readAuthorization reads a sign-in's authorization, null being none.
func readAuthorization(b *body, into **OAuthSignIn) error {
	if absent, err := null(b); absent || err != nil {
		return err
	}
	a := &OAuthSignIn{}
	if err := b.fields(a); err != nil {
		return err
	}
	*into = a
	return nil
}

// check refuses what the schema refuses of an authorization, before the client's document is asked
// for: each member present that has to be, the challenge a SHA-256, and a state within its bound.
func (a OAuthSignIn) check() error {
	switch {
	case a.ClientID == "" || a.RedirectURI == "" || a.Resource == "":
		return errors.New("authorization: an MCP client's authorisation carries client_id, redirect_uri, code_challenge and resource, as the page was opened with them")
	case !codeChallenge.MatchString(a.CodeChallenge):
		return errors.New("authorization: code_challenge is the SHA-256 of the client's verifier, 43 base64url characters, and this one is not")
	case !stateHolds(a.State):
		return fmt.Errorf("authorization: state is at most %d printable ASCII characters, as OAuth writes it", stateMost)
	}
	return nil
}

// stateHolds says whether a state is one OAuth's grammar writes, "1*VSCHAR", within stateMost, the
// empty one being none.
func stateHolds(state string) bool {
	if len(state) > stateMost {
		return false
	}
	for i := 0; i < len(state); i++ {
		if c := state[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// approval is an authorisation request that holds, as approve answers it: everything the code is
// minted with, and what the page shows of it.
type approval struct {
	client      clientDocument
	redirectURI string
	challenge   string

	// resource is the URL the token is for, and collection the collection's identifier where it is
	// one, empty for /mcp.
	resource, collection string

	scope []string
	state string
}

// clientRefused is an authorisation request whose client or redirect URI does not hold, which OAuth
// 2.1 answers on a page and never at the redirect URI: "the authorization server MUST NOT redirect
// the user agent to the invalid redirect URI and SHOULD inform the resource owner of the error".
type clientRefused struct{ why string }

func (e *clientRefused) Error() string { return e.why }

// oauthRefused is any other refusal of an authorisation request, which OAuth 2.1 sends the client at
// its redirect URI, as error and error_description, once the client and its redirect URI hold.
type oauthRefused struct{ code, why string }

func (e *oauthRefused) Error() string { return e.code + ": " + e.why }

// approve holds an authorisation request to its rules, the client's document fetched (or read from
// where it is kept) first, then the redirect URI against it, then the rest: what /auth/authorize
// holds a request to before it shows the page, and what the sign-in that allows it holds it to again.
// A *clientRefused is a page's refusal, answered with no approval; an *oauthRefused one sent to the
// client, answered beside the approval as far as it held, whose redirect URI and state say where; and
// any other error one of the installation's own.
func (s *OAuth) approve(ctx context.Context, a OAuthSignIn) (*approval, error) {
	if err := s.clientIDHolds(a.ClientID); err != nil {
		return nil, &clientRefused{why: err.Error()}
	}
	client, err := s.clients.get(ctx, a.ClientID)
	if err != nil {
		return nil, &clientRefused{why: err.Error()}
	}
	if !client.redirects(a.RedirectURI) {
		return nil, &clientRefused{why: "redirect_uri is not one of those the client's document names, and the browser is sent back to none other"}
	}
	ok := &approval{client: client, redirectURI: a.RedirectURI, challenge: a.CodeChallenge, state: a.State}
	switch {
	case !stateHolds(a.State):
		// Not handed back, since it is not one OAuth writes.
		ok.state = ""
		return ok, &oauthRefused{code: "invalid_request", why: fmt.Sprintf("state is at most %d printable ASCII characters", stateMost)}
	case !codeChallenge.MatchString(a.CodeChallenge):
		return ok, &oauthRefused{code: "invalid_request", why: "code_challenge is the SHA-256 of the client's verifier, 43 base64url characters, and this one is not"}
	}
	switch rest, collection := strings.CutPrefix(a.Resource, s.issuer+collectionsPath); {
	case a.Resource == s.issuer+mcpPath:
	case collection && ulidForm.MatchString(rest):
		ok.collection = rest
	default:
		return ok, &oauthRefused{code: "invalid_target", why: "resource is the URL of this installation's /mcp or of one of its collections, written as its metadata writes it"}
	}
	ok.resource = a.Resource
	used := mcpScopes
	if ok.collection != "" {
		used = collectionScopes
	}
	if a.Scope == "" {
		ok.scope = slices.Clone(used)
		return ok, nil
	}
	for _, asked := range strings.Split(a.Scope, " ") {
		switch {
		case !slices.Contains(used, asked):
			return ok, &oauthRefused{code: "invalid_scope", why: "scope names " + describable(asked) + ", which is no permission this resource uses: its scopes are " + strings.Join(used, ", ")}
		case slices.Contains(ok.scope, asked):
			return ok, &oauthRefused{code: "invalid_scope", why: "scope names " + describable(asked) + " twice"}
		}
		ok.scope = append(ok.scope, asked)
	}
	return ok, nil
}

// describable is a value a caller wrote as an error_description may repeat it: "Values for the
// error_description parameter MUST NOT include characters outside the set %x20-21 / %x23-5B /
// %x5D-7E", so one holding another, a quote or a backslash among them, or longer than a scope is, is
// named as a value rather than repeated.
func describable(v string) string {
	if v == "" || len(v) > 64 {
		return "a value"
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return "a value"
		}
	}
	return v
}

// clientIDHolds holds a client_id to the draft's Client Identifier URL: "MUST use the https URL scheme,
// MUST NOT contain a userinfo component, MAY contain a port, MUST contain a path component, MUST NOT
// contain single-dot or double-dot path components, SHOULD NOT contain a query component, MUST NOT
// contain a fragment component". A query is refused too, since nothing needs one and a document the
// installation fetches is one fewer place for a caller to write. One on the installation's own
// origin is refused, so that no client names itself after the installation and the API never fetches
// a document from itself: the draft has a server "reject a Client Identifier URL that is equal" to its
// own issuer, and an address under it is as confusing.
func (s *OAuth) clientIDHolds(id string) error {
	u, err := url.Parse(id)
	switch {
	case err != nil || len(id) > 2048:
		return errors.New("client_id is the https URL of the client's metadata document, and this one does not read as a URL")
	case u.Scheme != "https" || u.Host == "" || u.Opaque != "":
		return errors.New("client_id is the https URL of the client's metadata document, and this one is not https")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(id, "#"):
		return errors.New("client_id is an https URL with no user, query or fragment, and this one carries one")
	case u.Path == "" || u.Path == "/":
		return errors.New("client_id is an https URL with a path, where the client's metadata document is served, and this one has none")
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, "%2e") || strings.EqualFold(segment, "%2e%2e") || strings.EqualFold(segment, ".%2e") || strings.EqualFold(segment, "%2e.") {
			return errors.New("client_id holds a dot segment, which a client identifier URL never does")
		}
	}
	if o, err := originOf(id); err == nil && o == s.origin {
		return errors.New("client_id is an address of this installation's, and a client is somebody else's program")
	}
	return nil
}

// approveSignIn holds the authorization a sign-in carries to its rules again, as the sign-in routes
// do before anything is verified, and answers it, or nil beside the status and the sentence the
// sign-in is refused with: 400 for a request that no longer holds, which the page was served for when
// it did, and 409 where the installation issues no token, as for a ceremony an installation cannot run.
func approveSignIn(ctx context.Context, s *OAuth, a OAuthSignIn) (*approval, int, string) {
	if s == nil {
		return nil, http.StatusConflict, "authorization: this installation serves no MCP surface, and allows no client"
	}
	ok, err := s.approve(ctx, a)
	var client *clientRefused
	var refused *oauthRefused
	switch {
	case errors.As(err, &client):
		return nil, http.StatusBadRequest, "authorization: " + client.why
	case errors.As(err, &refused):
		return nil, http.StatusBadRequest, "authorization: " + refused.why
	case err != nil:
		return nil, http.StatusInternalServerError, "the client's authorisation could not be checked"
	}
	return ok, 0, ""
}

// handOff mints the code a sign-in that allows an MCP client hands it, in the transaction wide is, the
// one that opens the session, for login, who signed in with credential, and answers where the page
// sends the browser: the client's redirect URI with the code, its state and iss, "the issuer
// identifier of the authorization server" (RFC 9207). A collection the request names is held to be
// login's own here, once login is known, and one that is not is sent back as invalid_target rather
// than a code. The API writes the whole address rather than the page assembling it, as it does for agk
// login, "so that the page can never be made to send a code anywhere the API did not check".
func (s *OAuth) handOff(ctx context.Context, wide *db.Wide, a *approval, login, credential string, now time.Time) (string, error) {
	if a.collection != "" {
		_, err := wide.Collection(ctx, login, a.collection)
		if errors.Is(err, db.ErrNoCollection) {
			return s.refusedTo(a, &oauthRefused{code: "invalid_target", why: "resource names a collection that is not one of the account's"}), nil
		}
		if err != nil {
			return "", err
		}
	}
	code, _, err := token.New(token.Code, "")
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(code))
	if err := wide.IssueExchangeCode(ctx, db.ExchangeCode{
		Hash: hash[:], Login: login, Credential: credential, CodeChallenge: a.challenge,
		IssuedAt: now, ExpiresAt: now.Add(db.ExchangeCodeLife),
		OAuth: &db.CodeAuthorization{
			ClientID: a.client.ID, ClientName: a.client.Name, RedirectURI: a.redirectURI,
			Resource: a.resource, Scope: a.scope,
		},
	}); err != nil {
		return "", err
	}
	answer := url.Values{"code": {code}, "iss": {s.issuer}}
	if a.state != "" {
		answer.Set("state", a.state)
	}
	return withQuery(a.redirectURI, answer), nil
}

// refusedTo is the address that tells the client its request was refused: its redirect URI with
// error, error_description, its state where it wrote one that holds, and iss, "including error
// responses".
func (s *OAuth) refusedTo(a *approval, e *oauthRefused) string {
	answer := url.Values{"error": {e.code}, "error_description": {e.why}, "iss": {s.issuer}}
	if a.state != "" {
		answer.Set("state", a.state)
	}
	return withQuery(a.redirectURI, answer)
}

// withQuery is an address with parameters added to its query, after those it holds, "retaining" them
// as OAuth 2.1 asks of a redirect URI that has a query already.
func withQuery(address string, q url.Values) string {
	separator := "?"
	if strings.Contains(address, "?") {
		separator = "&"
	}
	return address + separator + q.Encode()
}
