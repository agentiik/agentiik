package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
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
}

// readSettings reads the API's configuration, then the master key and the env prefixes, which
// internal/config reads and leaves to the API to check. Every setting that refuses the start is
// named on the same start, these two among them.
func readSettings(lookup config.Lookup) (settings, error) {
	c, err := config.ReadAPI(lookup)
	refused := []error{err}
	s := settings{API: c}
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
func serve(ctx context.Context, s settings, ln net.Listener, log *slog.Logger) error {
	defer ln.Close()
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
	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()

	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go watchCredential(watching, s.Bus.Expires, log, time.Now, sleep)

	log.Info("serving", "address", ln.Addr().String(), "public_url", s.PublicURL)
	select {
	case err := <-served:
		return fmt.Errorf("the listener stopped: %w", err)
	case <-ctx.Done():
	}
	finishing, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(finishing); err != nil {
		log.Warn("the requests still being answered were cut", "after", shutdownGrace.String(), "error", err)
		server.Close()
	}
	<-served
	return nil
}

// installation is what serve answers with: the router, over what it holds open.
type installation struct {
	router *api.Router
	close  func()
}

// open connects to what the API stands on and builds every route on it.
func open(ctx context.Context, s settings, log *slog.Logger) (*installation, error) {
	operator, err := newOperator(s.OperatorToken)
	if err != nil {
		return nil, err
	}
	issuer, err := bus.NewIssuer(string(s.AccountSeed), s.Bus.URL)
	if err != nil {
		return nil, err
	}

	pool, err := db.Open(ctx, s.Database.ConnString())
	if err != nil {
		return nil, err
	}
	// The API's connection to the bus creates the streams, which it would otherwise wait for
	// the controller to, and each pool's consumer when a runner of that pool first asks for a
	// credential, since a runner may create neither.
	b, err := bus.Open(ctx, bus.Options{
		URL:         s.Bus.URL,
		Name:        program + " " + instanceName(os.Getpid()),
		Credentials: &bus.Credentials{JWT: s.Bus.JWT, Seed: string(s.Bus.Seed)},
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	closeAll := func() {
		b.Close()
		pool.Close()
	}

	router, err := routes(s, pool, b, issuer, operator, log)
	if err != nil {
		closeAll()
		return nil, err
	}
	return &installation{router: router, close: closeAll}, nil
}

// routes builds every route built so far on one router: runs and versions, the secret
// declarations, the runners and their pools, the bus credential, and the built-in object store.
func routes(s settings, pool *db.Pool, consumers api.BusConsumers, issuer api.BusIssuer, operator *operator, log *slog.Logger) (*api.Router, error) {
	rt, err := api.NewRouter(operator, operator.identify)
	if err != nil {
		return nil, err
	}

	// One directory for what a push writes, what a runner fetches and stores through a
	// presigned URL, and what a redemption reads, since the controller reads the same one.
	objects := artifact.Dir(s.Objects)
	// On the public URL rather than on a request's Host header, which a caller chooses. Outside
	// /api/v1, where api.NewObjects serves them.
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{Key: []byte(s.PresignKey), Base: s.PublicURL + "/objects"})
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
		BusIssuer: issuer, BusConsumers: consumers,
		Trouble: func(err error) {
			log.Warn("a redemption could not give a task a secret", "error", err)
		},
	}
	declarations := api.DeclarationOptions{Pool: pool}
	// The providers, attached here and nowhere else, since this is the one package allowed to
	// link the store. A nil keyring is not reachable, since the master key is required, and would
	// attach no built-in store rather than admit a value it could not seal.
	if err := secret.Attach(secret.Options{Pool: pool, Keys: s.keys, Environment: api.Environment(s.EnvPrefixes)}, &declarations, &runners); err != nil {
		return nil, err
	}

	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: versions, Objects: objects}); err != nil {
		return nil, err
	}
	if _, err := api.NewDeclarations(rt, declarations); err != nil {
		return nil, err
	}
	if _, err := api.NewRunners(rt, runners); err != nil {
		return nil, err
	}
	if _, err := api.NewObjects(rt, signed); err != nil {
		return nil, err
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

// credentialWarning is how long before the control plane's bus credential expires the API starts
// saying so: fourteen days, time enough for somebody to read the warning and renew it.
const credentialWarning = 14 * 24 * time.Hour

// credentialRepeat is how often it says so again inside that window, since a warning said once is
// a warning in a log nobody reads that day.
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

// watchCredential says when the control plane's bus credential nears its expiry, and when it
// passes it, until ctx is done.
//
// The API goes on serving past it, though what it serves narrows. Its bus connection is refused
// from then on, and every request for a runner's bus credential makes its pool's consumer ready
// on that connection first, so each is answered 500: a runner keeps the bus credential it holds,
// which is signed with the account seed and not with this one, until that expires within the
// hour, and loses the bus then. Its heartbeats are still heard throughout. An API that ended
// would stop those too, and the controller's first sweep after a restart would declare lost every
// task in flight, where one that goes on leaves the tasks to finish if the credential is renewed
// within the hour.
func watchCredential(ctx context.Context, expires time.Time, log *slog.Logger, now func() time.Time, wait func(context.Context, time.Duration) bool) {
	if expires.IsZero() {
		return
	}
	renew := "agentiik-api bus-credential on the directory holding the bus identity, then restart the API and the controller"
	for {
		left := expires.Sub(now())
		var next time.Duration
		switch {
		case left <= 0:
			log.Error("the control plane's bus credential has expired, and the bus refuses it: no runner is given a bus credential until it is renewed, so each loses the bus when the one it holds runs out, within the hour, and the controller, which holds the same credential, ends", "expired", expires.UTC().Format(time.RFC3339), "renew", renew)
			return
		case left <= credentialWarning:
			log.Warn("the control plane's bus credential expires soon", "expires", expires.UTC().Format(time.RFC3339), "left", left.Round(time.Minute).String(), "renew", renew)
			next = min(credentialRepeat, left)
		default:
			next = left - credentialWarning
		}
		if !wait(ctx, next) {
			return
		}
	}
}
