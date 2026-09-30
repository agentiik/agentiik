package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/console"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/secret"
	"github.com/agentiik/agentiik/version"
)

// settings are what serve runs on: the configuration internal/config read, with the two settings
// it could not read to the end finished here.
type settings struct {
	config.API

	// keys is the master key parsed, on a keyring of its own. Only this package may parse it,
	// since only the API may link the secret store that knows how.
	keys *secret.Keyring

	// now is the one clock every route tells time by, grants, tokens and sessions expiring by it,
	// and the wall clock where it is nil, which is what serve reads. A test sets it to hold the
	// installation at an instant, a grant's expiry, which the wall clock never waits at.
	now func() time.Time

	// console is the web console's build the API serves at the root of the public URL: what this
	// binary carries where AGK_CONSOLE leaves the console on, and nil where it is off or the build
	// skipped the console's stage, and then no console is served. A test sets a build of its own,
	// since the one this binary carries is whatever the machine running it last built.
	console fs.FS
}

// readSettings reads the API's configuration, then the master key and the env prefixes, which
// internal/config reads and leaves to the API to check. Every setting that refuses the start is
// named on the same start, these two among them.
func readSettings(lookup config.Lookup) (settings, error) {
	c, err := config.ReadAPI(lookup)
	refused := []error{err}
	s := settings{API: c}
	if c.Console {
		s.console = console.Files()
	}
	if c.MasterKey != "" {
		master, err := secret.ParseMaster([]byte(c.MasterKey))
		if err == nil {
			s.keys, err = secret.NewKeyring(master)
		}
		if err != nil {
			refused = append(refused, config.Refuse(config.MasterKeyFile, err))
		}
	}
	if err := api.Environment(c.EnvPrefixes).Check(); err != nil {
		refused = append(refused, config.Refuse(config.EnvPrefixes, err))
	}
	return s, errors.Join(refused...)
}

// serveVerb is agentiik-api serve.
func serveVerb(ctx context.Context, lookup config.Lookup, _, stderr io.Writer) int {
	renewExpired(lookup, time.Now(), stderr)
	s, err := readSettings(lookup)
	if err != nil {
		// One line per setting, each naming its variable, which is how config words them.
		fmt.Fprintf(stderr, "%s: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	return start(ctx, s, stderr)
}

// start serves on settings already read, and answers the exit code.
func start(ctx context.Context, s settings, stderr io.Writer) int {
	log := logger(stderr)
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s could not be listened on: %s\n", program, config.Listen, err)
		return exitFailed
	}
	if err := serve(ctx, s, ln, log); err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

// logger writes to w as text: the API's standard error is read by journald, docker logs or a
// person, and each of those reads text.
func logger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

// shutdownGrace is how long the requests being answered when a stop is asked for have to finish.
//
// Long enough for a push or a heartbeat, which take milliseconds. A download through a presigned
// URL can take longer than any bound worth waiting on, and is cut: the runner fetching it tries
// again, at this API or another, with the same URL.
const shutdownGrace = 30 * time.Second

// Timeouts on what a client sends before the API has anything to answer. None bounds a body or
// an answer, since an object is read and written through the object routes at whatever pace the
// network allows, and a route bounds its own body by size instead.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

// serve answers on ln until ctx is done, then finishes what it is answering and returns nil, or
// returns why it could not go on.
//
// Over TLS where the settings hold a certificate, and in plain HTTP to a terminator in front where
// they hold none. The listener is wrapped here rather than where it is opened, so that what a test
// hands serve is served as main serves it.
func serve(ctx context.Context, s settings, ln net.Listener, log *slog.Logger) error {
	defer ln.Close()
	served, err := s.TLS.Server()
	if err != nil {
		return config.Refuse(config.TLSCertFile, err)
	}
	if served != nil {
		// A client certificate asked for and never required, and verified against no authority:
		// a webhook authenticating by mtls holds a caller to the one certificate written for it,
		// which the handshake proved the caller holds the key of, and every other route reads
		// none. Asked here, of the TLS the API serves itself, since a certificate a terminator in
		// front forwards in a header is one a client could write there too.
		served.ClientAuth = tls.RequestClientCert
		ln = tls.NewListener(ln, served)
	}
	in, err := open(ctx, s, log)
	switch {
	case err != nil && ctx.Err() != nil:
		// A stop asked for while the database or the bus was still being reached, which
		// is what a stack still coming up looks like: the failure is the stop's, not the
		// installation's, and the program says nothing of it and exits 0, as the
		// controller does.
		return nil
	case err != nil:
		return err
	}
	defer in.close()

	server := &http.Server{
		Handler:           in.router,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(ln) }()

	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go watchCredential(watching, expiry(s.Bus), renewer(in.issuer, s.Bus.CredentialsFile, time.Now), log, time.Now, sleep)

	log.Info("serving", "address", ln.Addr().String(), "tls", s.TLS.Served(), "public_url", s.PublicURL, "console", consoleState(s), "mcp", mcpState(s))
	select {
	case err := <-stopped:
		return fmt.Errorf("the listener stopped: %w", err)
	case <-ctx.Done():
	}
	finishing, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(finishing); err != nil {
		log.Warn("the requests still being answered were cut", "after", shutdownGrace.String(), "error", err)
		server.Close()
	}
	<-stopped
	return nil
}

// consoleState is what the start says of the console: served, off as AGK_CONSOLE asked, or absent
// from a build that skipped its stage, which serves every route but the console, as the page says,
// and says so once rather than leave somebody reading a 404 at the root to guess which.
func consoleState(s settings) string {
	switch {
	case s.console != nil:
		return "served"
	case !s.Console:
		return "off"
	default:
		return "not carried by this build"
	}
}

// mcpState is what the start says of the MCP endpoints: served, or off as AGK_MCP asked, so that
// somebody reading a 404 at /mcp is not left to guess why.
func mcpState(s settings) string {
	if s.MCP {
		return "served"
	}
	return "off"
}

// installation is what serve answers with: the router, over what it holds open.
type installation struct {
	router *api.Router
	close  func()

	// issuer mints with the account seed, a runner's bus credential and the control plane's
	// renewed one.
	issuer *bus.Issuer

	// pool is the database the routes are served on, kept for a test to ask what its sessions
	// are.
	pool *db.Pool
}

// open connects to what the API stands on and builds every route on it.
func open(ctx context.Context, s settings, log *slog.Logger) (*installation, error) {
	issuer, err := bus.NewIssuer(string(s.AccountSeed), s.Bus.URL)
	if err != nil {
		return nil, err
	}

	// The server probes the API's sessions, as db.Keepalives says why: an act holds the head of the
	// audit log's chain from its append to its commit, and an API cut off in between would
	// otherwise hold every other act of the installation for the two hours the operating system
	// waits.
	pool, err := db.Open(ctx, db.WithKeepalives(s.Database.ConnString()))
	if err != nil {
		return nil, err
	}
	// The API's connection to the bus creates the streams, which it would otherwise wait for
	// the controller to, and each pool's consumer, since a runner may create neither: every
	// pool's here, the default pool the installation was migrated with among them, and a new
	// pool's when it is created. Never when a runner asks for its bus credential, so that
	// asking still works once this connection's own credential has expired.
	b, err := bus.Open(ctx, bus.Options{
		URL:         s.Bus.URL,
		Name:        program + " " + instanceName(os.Getpid()),
		Credentials: &bus.Credentials{JWT: s.Bus.JWT, Seed: string(s.Bus.Seed)},
		Reread:      rereadBus(s.Bus),
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	closeAll := func() {
		b.Close()
		pool.Close()
	}
	if err := api.ReadyQueues(ctx, pool, b); err != nil {
		closeAll()
		return nil, err
	}

	// The log streams end when the stop is asked for rather than when the grace runs out, so
	// that their readers reconnect to another API at once.
	router, err := routes(s, pool, b, issuer, log, ctx.Done())
	if err != nil {
		closeAll()
		return nil, err
	}
	return &installation{router: router, close: closeAll, pool: pool, issuer: issuer}, nil
}

// routes builds every route built so far on one router: runs and versions, the step log streams,
// the secret declarations, the runners and their pools, the bus credential, the users and groups,
// the namespaces, the API tokens, the grants, the caller's own record, the built-in object store,
// the service accounts, the passkey ceremonies, the passwords, agk login's exchange, the
// authentication policy, the caller's credentials, and the sign-in page with its sign-out. Each
// request is identified and authorised by api.Principals, from the tokens, the grants and the
// bootstrap state the database holds. The log streams end when stopping closes. Every route is
// handed the settings' one clock, so that a grant lapses at the same instant for the authorizer,
// the routes that list grants and GET /api/v1/me.
func routes(s settings, pool *db.Pool, consumers api.BusConsumers, issuer api.BusIssuer, log *slog.Logger, stopping <-chan struct{}) (*api.Router, error) {
	principals, err := api.NewPrincipals(pool, s.now)
	if err != nil {
		return nil, err
	}
	// The console's sessions, whose requests changing something come from the pages of the public
	// URL alone, the one origin the sign-in page is served on.
	if err := principals.AcceptSessions(s.PublicURL); err != nil {
		return nil, err
	}
	rt, err := api.NewRouter(principals, principals.Identify)
	if err != nil {
		return nil, err
	}

	// One directory for what a push writes, what a runner fetches and stores through a
	// presigned URL, and what a redemption reads, since the controller reads the same one.
	objects := artifact.Dir(s.Objects)
	// On the public URL rather than on a request's Host header, which a caller chooses. Outside
	// /api/v1, where api.NewObjects serves them.
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{Key: []byte(s.PresignKey), Base: s.PublicURL + "/objects", Now: s.now})
	if err != nil {
		return nil, err
	}
	versions, err := version.New(pool, version.Options{})
	if err != nil {
		return nil, err
	}

	runners := api.RunnerOptions{
		Pool: pool, JoinRotation: s.JoinRotation, RevocationGrace: s.RevocationGrace,
		Objects: objects, URLs: signed,
		BusIssuer: issuer, BusConsumers: consumers, Now: s.now,
		Trouble: func(err error) {
			log.Warn("a redemption could not give a task a secret", "error", err)
		},
	}
	declarations := api.DeclarationOptions{Pool: pool, Now: s.now}
	// The providers, attached here and nowhere else, since this is the one package allowed to
	// link the store. A nil keyring is not reachable, since the master key is required, and would
	// attach no built-in store rather than admit a value it could not seal.
	if err := secret.Attach(secret.Options{Pool: pool, Keys: s.keys, Environment: api.Environment(s.EnvPrefixes)}, &declarations, &runners); err != nil {
		return nil, err
	}

	// The webhooks' secrets are sealed under the master key, as a built-in secret's value is. A nil
	// keyring is not reachable, and would take no secret and refuse every hmac request.
	var hooks api.HookSecrets
	if s.keys != nil {
		sealed, err := secret.NewHooks(s.keys)
		if err != nil {
			return nil, err
		}
		hooks = sealed
	}
	if _, err := api.NewServer(rt, api.ServerOptions{
		Pool: pool, Versions: versions, Objects: objects, URLs: signed, Stopping: stopping, Now: s.now, PublicURL: s.PublicURL,
		Hooks:   hooks,
		Trouble: func(err error) { log.Warn("a request to the API met trouble that is the installation's", "error", err) },
	}); err != nil {
		return nil, err
	}
	if _, err := api.NewDeclarations(rt, declarations); err != nil {
		return nil, err
	}
	if _, err := api.NewRunners(rt, runners); err != nil {
		return nil, err
	}
	// The users and groups, whose enrolment links point at the enrolment page on the public URL,
	// the Relying Party's origin.
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: pool, PublicURL: s.PublicURL, Now: s.now}); err != nil {
		return nil, err
	}
	if _, err := api.NewNamespaces(rt, api.NamespaceOptions{Pool: pool}); err != nil {
		return nil, err
	}
	// Every write held to its namespace's max_artifact_bytes.
	if _, err := api.NewObjects(rt, signed, pool); err != nil {
		return nil, err
	}
	// The API tokens of whoever asks, and of the service accounts of the namespaces they own.
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool, Now: s.now}); err != nil {
		return nil, err
	}
	// The grants of each namespace and workflow, and who the caller is, with what it is told.
	if _, err := api.NewSharing(rt, api.SharingOptions{Pool: pool, PublicURL: s.PublicURL, Now: s.now}); err != nil {
		return nil, err
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: pool, Now: s.now}); err != nil {
		return nil, err
	}
	// The service accounts themselves, created, listed and removed by whoever owns their namespace.
	if _, err := api.NewServiceAccounts(rt, api.ServiceAccountOptions{Pool: pool}); err != nil {
		return nil, err
	}
	// The passkey ceremonies, on the public URL's host as the Relying Party, reading a session
	// that may only enrol, which the router refuses everywhere else; and the password sign-in,
	// opening a TOTP generator's secret with the master key, with the routes that set a password,
	// from an enrolment code or a session, one that may only enrol included, and enrol a TOTP
	// generator beside it. The two share where a sign-in comes from, the proxy's X-Forwarded-For
	// behind AGK_PROXY_URL, and the bound on the failures they record.
	signIns := api.NewSignIns(s.Proxied)
	if _, err := api.NewPasskeys(rt, api.PasskeyOptions{
		Pool: pool, PublicURL: s.PublicURL, Identify: principals.Identify, SignIns: signIns, Now: s.now,
		Trouble: func(err error) { log.Warn("a failed sign-in could not be recorded in the audit log", "error", err) },
	}); err != nil {
		return nil, err
	}
	passwords := api.PasswordOptions{
		Pool: pool, PublicURL: s.PublicURL, SignIns: signIns, Identify: principals.Identify, Now: s.now,
		Trouble: func(err error) { log.Warn("a password sign-in was answered with trouble", "error", err) },
	}
	// A nil keyring is not reachable, since the master key is required, and would open no TOTP
	// generator, which refuses the sign-in of an account holding one rather than admit it with its
	// password alone, and would enrol none.
	if s.keys != nil {
		totp, err := secret.NewTOTP(s.keys)
		if err != nil {
			return nil, err
		}
		passwords.TOTP = totp
	}
	if _, err := api.NewPasswords(rt, passwords); err != nil {
		return nil, err
	}
	// agk login's exchange, trading the one-time code either sign-in hands its loopback address for
	// an API token, its refusals recorded within the bound the sign-ins share.
	if _, err := api.NewExchange(rt, api.ExchangeOptions{
		Pool: pool, PublicURL: s.PublicURL, SignIns: signIns, Now: s.now,
		Trouble: func(err error) { log.Warn("a refused exchange could not be recorded in the audit log", "error", err) },
	}); err != nil {
		return nil, err
	}
	// The authentication policy, the installation's and each namespace's tightening of it, which an
	// administrator sets; and the caller's own credentials, listed and removed as the policy allows.
	if _, err := api.NewPolicies(rt, api.PolicyOptions{Pool: pool, PublicURL: s.PublicURL, Now: s.now}); err != nil {
		return nil, err
	}
	if _, err := api.NewCredentials(rt, api.CredentialOptions{Pool: pool, PublicURL: s.PublicURL, Now: s.now}); err != nil {
		return nil, err
	}
	// The sign-in and enrolment page the ceremonies run on, and the sign-out it offers, with the
	// password forms where the policy lets passwords in, now that the password routes are served.
	if _, err := api.NewSignIn(rt, api.SignInOptions{Pool: pool, PublicURL: s.PublicURL, Sessions: principals, Passwords: true, Now: s.now}); err != nil {
		return nil, err
	}
	// The web console, at every address outside the API's roots, where there is one to serve.
	if s.console != nil {
		if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: s.console, PublicURL: s.PublicURL}); err != nil {
			return nil, err
		}
	}
	// The platform's MCP server at /mcp, unless AGK_MCP is off, from this process: "MCP deploys no
	// new component and opens no new port."
	if s.MCP {
		if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: s.PublicURL, Version: moduleVersion()}); err != nil {
			return nil, err
		}
	}
	return rt, nil
}

// instanceName is what an operator reads in the bus's list of connections, the host and the
// process, since two APIs of one installation on one host are told apart by the second.
func instanceName(pid int) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, pid)
}

// credentialWarning is how long before the control plane's bus credential expires the API renews it,
// and says so where it cannot: fourteen days, time enough for somebody to read the warning and put
// right what stops the renewal, or renew it themselves.
const credentialWarning = 14 * 24 * time.Hour

// credentialRepeat is how often the API looks at the credential again: at most a day apart, so that
// one replaced in its file by a shorter one is renewed within the day, and a warning is said again
// every day it holds, since a warning said once is a warning in a log nobody reads that day.
const credentialRepeat = 24 * time.Hour

// sleep waits for d, and answers false where ctx was done first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// rereadBus reads the control plane's credential again from its file, which the bus connection
// does at every reconnection, so that it comes back with a credential renewed while the API runs.
func rereadBus(b config.Bus) func() (bus.Credentials, error) {
	return func() (bus.Credentials, error) {
		renewed, err := config.RereadBus(b)
		if err != nil {
			return bus.Credentials{}, err
		}
		return bus.Credentials{JWT: renewed.JWT, Seed: string(renewed.Seed)}, nil
	}
}

// expiry answers when the control plane's credential in its file expires, read again at every
// call, and when the one read last did where the file no longer reads. It is called by one
// goroutine alone.
func expiry(b config.Bus) func() time.Time {
	return func() time.Time {
		if renewed, err := config.RereadBus(b); err == nil {
			b = renewed
		}
		return b.Expires
	}
}

// watchCredential renews the control plane's bus credential from fourteen days before it expires,
// looking at it at start and every day after, and says when it cannot, until ctx is done. expiry
// answers when it expires, from its file, at every wake; renew renews it, and answers when the new
// one expires, and is nil where nothing may.
//
// A credential renewed in its file before the old one expires is taken with no restart: the bus
// drops the API's connection when the old one expires, and the connection comes back with the
// renewed one, as rereadBus reads it, and the controller's likewise. One renewed only after the old
// one expired needs a restart of the API, since its connection gave up at the bus's second
// refusal, while the controller, which ended then, takes it at its own restart.
//
// The API goes on serving past the expiry, though what it serves narrows. Its bus connection is
// refused from then on, so a runner pool can no longer be created, since its consumer is made ready
// on that connection. A runner is still given its bus credential, which is signed with the account
// seed and not with this one, so the runners keep the bus and finish what they hold, and their
// heartbeats are still heard. An API that ended would stop both, and the controller's first sweep
// after a restart would declare lost every task in flight. The controller holds the same
// credential and ends, so nothing new is dispatched until it is renewed.
func watchCredential(ctx context.Context, expiry func() time.Time, renew func() (time.Time, error), log *slog.Logger, now func() time.Time, wait func(context.Context, time.Duration) bool) {
	fix := "let the API write to the directory holding the file " + config.BusCredentialsFile + " names, or run agentiik-api init, before it expires: the API and the controller take the renewed credential from their files when the bus drops the old one, with no restart"
	for {
		expires := expiry()
		if expires.IsZero() {
			return
		}
		left := expires.Sub(now())
		if left <= credentialWarning && renew != nil {
			renewed, err := renew()
			switch {
			case err == nil && left <= 0:
				log.Warn("renewed the control plane's bus credential, which had expired already: restart the API, whose bus connection gave up at the bus's refusal, while the controller takes it as it starts again", "expired", expires.UTC().Format(time.RFC3339), "expires", renewed.UTC().Format(time.RFC3339))
				expires, left = renewed, renewed.Sub(now())
			case err == nil:
				log.Info("renewed the control plane's bus credential", "was_to_expire", expires.UTC().Format(time.RFC3339), "expires", renewed.UTC().Format(time.RFC3339))
				expires, left = renewed, renewed.Sub(now())
			default:
				log.Warn("the control plane's bus credential could not be renewed", "error", err)
			}
		}
		next := credentialRepeat
		switch {
		case left <= 0:
			log.Error("the control plane's bus credential has expired, and the bus refuses it: the controller, which holds the same credential, ends, so nothing is dispatched until it is renewed, and no runner pool can be created, while the runners are still given their bus credentials and finish what they hold", "expired", expires.UTC().Format(time.RFC3339), "renew", "let the API write to the directory holding the file "+config.BusCredentialsFile+" names, or run agentiik-api init, then restart the API and the controller")
			if renew == nil {
				return
			}
		case left <= credentialWarning:
			log.Warn("the control plane's bus credential expires soon", "expires", expires.UTC().Format(time.RFC3339), "left", left.Round(time.Minute).String(), "renew", fix)
			next = min(credentialRepeat, left)
		default:
			next = min(credentialRepeat, left-credentialWarning)
		}
		if !wait(ctx, next) {
			return
		}
	}
}
