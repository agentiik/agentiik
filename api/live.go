package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentiik/agentiik/db"
	"github.com/coder/websocket"
)

// The live connection, GET /api/v1/me/live: a WebSocket telling its caller, as it happens, what
// changed among what it may read, so that a console reads again what it shows when it changes
// rather than on a clock.
//
// A message names what changed and holds nothing of it:
//
//	{"kind": "run", "namespace", "workflow", "run"}   a run, one of its steps or tasks, under run:read
//	{"kind": "notifications"}                         the caller's notifications
//	{"kind": "runners"}                               a runner or a pool, to an administrator
//	{"kind": "activity"}                              a run anywhere, to an administrator, naming none
//	{"kind": "all"}                                   anything: a change may have been missed
//
// What it names is read through the route that answers it, under that route's permissions, so that
// who may read what is decided where it always was, and a message missed costs a read rather than
// a wrong screen. The changes come from the triggers of migration 0067, through one connection per
// API listening on db.LiveChannel for every live connection it serves, as the log streams' does.

// liveTiming is how a live connection spends its time, an argument so that a test has short ones.
type liveTiming struct {
	// pace is how long changes are gathered before they are sent, each once; ping how often the
	// connection is pinged, which a proxy counts as traffic; reauthorise how often the credential
	// is identified again and the permissions asked again; and write how long one message or one
	// ping may take before the connection is let go of.
	pace, ping, reauthorise, write time.Duration
}

// The defaults.
//
// livePace gathers what a busy run says in a quarter of a second, a fan-out's shards moving
// together among it, into one message per run, and is below what a reader notices.
//
// The ping is the log stream's keep-alive, below the sixty seconds of idle nginx and an AWS load
// balancer allow, and a permission revoked stops its messages within the log stream's 30 s, for
// the reason that one stops its lines.
var defaultLiveTiming = liveTiming{pace: 250 * time.Millisecond, ping: logKeepAlive, reauthorise: logReauthorise, write: 10 * time.Second}

// changedActivity is the message telling an administrator that a run changed somewhere, which GET
// /api/v1/stats/activity counts. It names nothing, since an administrator may read no namespace's
// runs: what they are told is that the installation's load moved, not where.
const changedActivity db.ChangeKind = "activity"

// liveQueue is how many changes wait for one connection to take them. A connection that falls
// further behind is told everything changed, once it catches up, rather than holding the others
// back: the listener never waits on a connection.
const liveQueue = 256

// liveMessage is one message of the live connection, openapi.json's liveChange.
type liveMessage struct {
	Kind      db.ChangeKind `json:"kind"`
	Namespace string        `json:"namespace,omitempty"`
	Workflow  string        `json:"workflow,omitempty"`
	Run       string        `json:"run,omitempty"`
}

// liveHub hands every change the API hears to every live connection it serves, listening only
// while one is open.
type liveHub struct {
	pool *db.Pool

	mu   sync.Mutex
	subs map[*liveSub]bool
	stop context.CancelFunc
}

// liveSub is one connection's place at the hub: the changes it has not taken yet, and whether any
// were dropped because it had not.
type liveSub struct {
	changes chan db.LiveChange
	missed  atomic.Bool
}

// join gives a connection its place, listening if nobody was, until the function it answers is
// called.
func (h *liveHub) join() (*liveSub, func()) {
	sub := &liveSub{changes: make(chan db.LiveChange, liveQueue)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[*liveSub]bool{}
	}
	h.subs[sub] = true
	if h.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.stop = cancel
		go h.listen(ctx)
	}
	return sub, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, sub)
		if len(h.subs) == 0 && h.stop != nil {
			h.stop()
			h.stop = nil
		}
	}
}

// listen holds the listening connection until ctx is done, and takes another when one fails. Each
// time it listens again it says everything changed, which db.WatchLive does first, since what was
// said meanwhile was heard by nobody.
func (h *liveHub) listen(ctx context.Context) {
	for {
		h.pool.WatchLive(ctx, h.deliver)
		if ctx.Err() != nil {
			return
		}
		pause := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			pause.Stop()
			return
		case <-pause.C:
		}
	}
}

// deliver hands a change to every connection, dropping it for one whose queue is full and marking
// that one as having missed something.
func (h *liveHub) deliver(change db.LiveChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.changes <- change:
		default:
			sub.missed.Store(true)
		}
	}
}

// live answers GET /api/v1/me/live.
func (s *Server) live(w http.ResponseWriter, r *http.Request, who Principal, _ Target, holds Holds) {
	// The handshake is checked here rather than left to the WebSocket library, so that a refusal
	// is the API's, in JSON with a sentence, as every other is.
	if !headerHolds(r.Header, "Connection", "upgrade") || !headerHolds(r.Header, "Upgrade", "websocket") {
		w.Header().Set("Upgrade", "websocket")
		fail(w, http.StatusUpgradeRequired, "GET /api/v1/me/live is a WebSocket: the request asks for the connection to be upgraded, with Upgrade: websocket and Connection: Upgrade")
		return
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		fail(w, http.StatusBadRequest, "the live connection speaks version 13 of the WebSocket protocol, RFC 6455's, and the handshake asked for another")
		return
	}
	if keys := r.Header.Values("Sec-WebSocket-Key"); len(keys) != 1 || !webSocketKey(keys[0]) {
		fail(w, http.StatusBadRequest, "a WebSocket handshake carries one Sec-WebSocket-Key, 16 bytes in base64, as RFC 6455 has it")
		return
	}
	// A session's handshake comes from the installation's own pages: a browser sends the cookie of
	// a page of any site sharing the installation's domain, SameSite=Lax letting it, and a GET is
	// never held to its origin by the router. A bearer token is no browser's, which no page of
	// another site can send.
	if _, bearer := bearerOf(r); !bearer {
		origin, err := originOf(s.publicURL)
		if origins := r.Header.Values("Origin"); err != nil || len(origins) != 1 || origins[0] != origin {
			fail(w, http.StatusForbidden, "a session opens the live connection from the installation's own pages, whose Origin its handshake carries")
			return
		}
	}

	// The origin was held to the public URL above, rather than to the Host the request names,
	// which a proxy may write as it pleases.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	sub, leave := s.liveHub.join()
	defer leave()

	// Nothing is read from the client but the protocol's own frames, which reading answers, a pong
	// among them: a message from it closes the connection with 1008, and ctx ends when the
	// connection does. Read here rather than by the library's CloseRead, whose goroutine closing
	// the connection waits fifteen seconds on itself.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		if _, _, err := c.Read(ctx); err == nil {
			c.Close(websocket.StatusPolicyViolation, "the live connection takes no message from its client")
		}
	}()
	(&liveFollower{server: s, conn: c, who: who, holds: holds, still: Still(r), administers: Administers(r)}).follow(ctx, sub)
}

// liveFollower is one live connection's caller, and what it was last answered about them.
type liveFollower struct {
	server      *Server
	conn        *websocket.Conn
	who         Principal
	holds       Holds
	still       func(context.Context) (bool, error)
	administers func(context.Context) (bool, error)

	// reads and administering are what the authorizer answered since it was last asked again:
	// whether the caller reads the runs of each workflow, and whether it administers.
	reads         map[Target]bool
	administering *bool
}

// follow sends the changes the caller may read until the connection ends, the API stops, or the
// credential no longer identifies its caller.
func (f *liveFollower) follow(ctx context.Context, sub *liveSub) {
	timing := f.server.liveTiming
	ping := time.NewTicker(timing.ping)
	defer ping.Stop()
	reauthorise := time.NewTicker(timing.reauthorise)
	defer reauthorise.Stop()
	gather := time.NewTimer(timing.pace)
	gather.Stop()
	gathering := false
	pending := map[liveMessage]bool{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-f.server.stopping:
			f.conn.Close(websocket.StatusGoingAway, "the API is stopping: connect again")
			return
		case <-ping.C:
			pinging, stop := context.WithTimeout(ctx, timing.write)
			err := f.conn.Ping(pinging)
			stop()
			if err != nil {
				return
			}
		case <-reauthorise.C:
			if still, err := f.still(ctx); err != nil || !still {
				f.conn.Close(websocket.StatusPolicyViolation, "the credential no longer identifies its caller")
				return
			}
			f.reads, f.administering = nil, nil
		case change := <-sub.changes:
			if sub.missed.Swap(false) {
				change = db.LiveChange{Kind: db.ChangedAll}
			}
			messages := f.shown(ctx, change)
			if len(messages) == 0 {
				continue
			}
			for _, message := range messages {
				if message.Kind == db.ChangedAll {
					clear(pending)
				}
				if !pending[liveMessage{Kind: db.ChangedAll}] {
					pending[message] = true
				}
			}
			if !gathering {
				gather.Reset(timing.pace)
				gathering = true
			}
		case <-gather.C:
			gathering = false
			for message := range pending {
				if err := f.send(ctx, message); err != nil {
					return
				}
			}
			clear(pending)
		}
	}
}

// shown is what a change is to the caller: none where it may read nothing of it, and for a run,
// the run where it reads its workflow's runs and the installation's activity where it administers,
// either or both.
func (f *liveFollower) shown(ctx context.Context, change db.LiveChange) []liveMessage {
	switch change.Kind {
	case db.ChangedRun:
		var told []liveMessage
		over := Target{Namespace: change.Namespace, Workflow: change.Workflow}
		reads, asked := f.reads[over]
		if !asked {
			var err error
			if reads, err = f.holds(ctx, over); err == nil {
				if f.reads == nil {
					f.reads = map[Target]bool{}
				}
				f.reads[over] = reads
			}
		}
		if reads {
			told = append(told, liveMessage{Kind: db.ChangedRun, Namespace: change.Namespace, Workflow: change.Workflow, Run: change.Run})
		}
		if f.administrator(ctx) {
			told = append(told, liveMessage{Kind: changedActivity})
		}
		return told
	case db.ChangedNotifications:
		if Principal(change.Recipient) == f.who {
			return []liveMessage{{Kind: db.ChangedNotifications}}
		}
	case db.ChangedRunners:
		if f.administrator(ctx) {
			return []liveMessage{{Kind: db.ChangedRunners}}
		}
	case db.ChangedAll:
		return []liveMessage{{Kind: db.ChangedAll}}
	}
	return nil
}

// administrator is whether the caller administers the installation, as the authorizer last
// answered it, and false where it could not answer.
func (f *liveFollower) administrator(ctx context.Context) bool {
	if f.administering == nil {
		administers, err := f.administers(ctx)
		if err != nil {
			return false
		}
		f.administering = &administers
	}
	return *f.administering
}

// send writes one message, within the time one may take.
func (f *liveFollower) send(ctx context.Context, message liveMessage) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	writing, stop := context.WithTimeout(ctx, f.server.liveTiming.write)
	defer stop()
	return f.conn.Write(writing, websocket.MessageText, body)
}

// headerHolds says whether a header lists a token, as Connection and Upgrade list theirs, in any
// case.
func headerHolds(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, t := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// webSocketKey says whether a Sec-WebSocket-Key is 16 bytes in base64, as RFC 6455 has it.
func webSocketKey(key string) bool {
	decoded, err := base64.StdEncoding.DecodeString(key)
	return err == nil && len(decoded) == 16
}
