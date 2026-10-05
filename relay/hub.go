package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

const (
	helloTimeout     = 10 * time.Second
	writeTimeout     = 10 * time.Second
	idleTimeout      = 75 * time.Second // clients ping every 20s
	sendQueue        = 256
	maxFrameBytes    = 256 << 10
	maxTextBytes     = 32 << 10
	maxHops          = 4
	defaultTTL       = 180
	minTTL           = 30
	maxTTL           = 3600
	maxClaimBatch    = 50
	janitorInterval  = 10 * time.Second
	messageRetention = 7 * 24 * time.Hour
	cursorRetention  = 30 * 24 * time.Hour
	// Mail a machine never received waits this long for that machine's next
	// main agent (every omp session connects under a new peer id).
	undeliveredWindow = 24 * time.Hour
)

var guestResource = regexp.MustCompile(`^[A-Za-z0-9._/*?:@+-]+$`)

// Per-role limits. Guests are someone else's machines: they get less
// throughput, shorter leases, no interrupting, and no force-release.
type limits struct {
	sendsPerMinute int
	maxTTL         int
	maxClaims      int
	urgent         bool
	forceRelease   bool
}

var roleLimits = map[string]limits{
	roleOwner: {sendsPerMinute: 120, maxTTL: maxTTL, maxClaims: 500, urgent: true, forceRelease: true},
	roleGuest: {sendsPerMinute: 20, maxTTL: 600, maxClaims: 20},
}

// PeerInfo is what a client says about itself. The relay checks Machine
// against its principal; ID is "<machine>/<root session>/<agent>" and the
// first two segments form the peer's family, which shares claims.
type PeerInfo struct {
	ID      string `json:"id"`
	Machine string `json:"machine"`
	Role    string `json:"role"` // agent role: "main" | "sub"
	Name    string `json:"name,omitempty"`
	Parent  string `json:"parent,omitempty"`
}

type PeerState struct {
	PeerInfo
	Trust       string `json:"trust"` // principal role, stamped by the relay
	Busy        bool   `json:"busy"`
	Task        string `json:"task,omitempty"`
	ConnectedAt int64  `json:"connectedAt"`
	LastSeen    int64  `json:"lastSeen"`
}

func family(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		if j := strings.Index(id[i+1:], "/"); j >= 0 {
			return id[:i+1+j]
		}
	}
	return id
}

func sameFamily(a, b string) bool { return family(a) == family(b) }

// matchTarget: exact peer id, or a pattern where '*' matches any run of
// characters ("*" everyone, "pcB/*" one machine, "*/main" every main agent).
func matchTarget(pattern, id string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == id
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(id, parts[0]) {
		return false
	}
	rest := id[len(parts[0]):]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(rest, p)
		}
		k := strings.Index(rest, p)
		if k < 0 {
			return false
		}
		rest = rest[k+len(p):]
	}
	return true
}

type Request struct {
	ID        int64       `json:"id"`
	Op        string      `json:"op"`
	Token     string      `json:"token,omitempty"`
	Repo      string      `json:"repo,omitempty"`
	Peer      *PeerInfo   `json:"peer,omitempty"`
	To        string      `json:"to,omitempty"`
	Text      string      `json:"text,omitempty"`
	Urgent    bool        `json:"urgent,omitempty"`
	Hops      int         `json:"hops,omitempty"`
	Seq       int64       `json:"seq,omitempty"`
	Resources []string    `json:"resources,omitempty"`
	TTL       int         `json:"ttl,omitempty"`
	Fence     int64       `json:"fence,omitempty"`
	SHA       string      `json:"sha,omitempty"`
	Note      string      `json:"note,omitempty"`
	Force     bool        `json:"force,omitempty"`
	Busy      *bool       `json:"busy,omitempty"`
	Task      *string     `json:"task,omitempty"`
	Title     string      `json:"title,omitempty"`
	Notes     string      `json:"notes,omitempty"`
	Patch     *BoardPatch `json:"patch,omitempty"`
}

type obj = map[string]any

type conn struct {
	ws        *websocket.Conn
	room      string
	principal *Principal
	state     PeerState
	out       chan []byte
	cancel    context.CancelFunc
}

func (c *conn) actor() Actor {
	return Actor{Peer: c.state.ID, Principal: c.principal.Name, Role: c.principal.Role}
}

type Hub struct {
	store *Store

	mu    sync.Mutex
	rooms map[string]map[string]*conn
	sends map[string][]time.Time // per principal, for rate limiting
}

func newHub(store *Store) *Hub {
	return &Hub{store: store, rooms: map[string]map[string]*conn{}, sends: map[string][]time.Time{}}
}

func validID(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// enqueue never blocks the hub: a peer that cannot keep up is disconnected
// and recovers its backlog from the message log on reconnect.
func (c *conn) enqueue(frame []byte) {
	select {
	case c.out <- frame:
	default:
		c.cancel()
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// broadcastLocked sends an event to every peer in a room. Caller holds h.mu.
func (h *Hub) broadcastLocked(room string, ev obj) {
	frame := mustJSON(ev)
	for _, c := range h.rooms[room] {
		c.enqueue(frame)
	}
}

// peersLocked is the roster as viewer may see it: guests see every main
// agent but only their own machine's subagents, and no other machine's task
// summary (those come from the owner's prompts).
func (h *Hub) peersLocked(room string, viewer *conn) []PeerState {
	out := []PeerState{}
	for _, c := range h.rooms[room] {
		st := c.state
		if viewer.principal.Role == roleGuest && c.principal.Name != viewer.principal.Name {
			if st.Role == "sub" {
				continue
			}
			st.Task = ""
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) broadcastPresenceLocked(room string) {
	for _, c := range h.rooms[room] {
		c.enqueue(mustJSON(obj{"ev": "presence", "peers": h.peersLocked(room, c)}))
	}
}

func (h *Hub) broadcastClaimsLocked(room string) {
	claims, err := h.store.claims(room)
	if err != nil {
		log.Printf("room %s: list claims: %v", room, err)
		return
	}
	h.broadcastLocked(room, obj{"ev": "claims", "claims": claims})
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxFrameBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c, err := h.handshake(ctx, ws, cancel)
	if err != nil {
		ws.Close(websocket.StatusPolicyViolation, truncate(err.Error(), 120))
		return
	}
	defer h.disconnect(c)

	go h.writer(ctx, c)
	for {
		readCtx, done := context.WithTimeout(ctx, idleTimeout)
		_, data, err := ws.Read(readCtx)
		done()
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(data, &req); err != nil {
			c.enqueue(mustJSON(obj{"id": 0, "ok": false, "error": "malformed frame"}))
			continue
		}
		reply := h.handle(c, &req)
		reply["id"] = req.ID
		c.enqueue(mustJSON(reply))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (h *Hub) writer(ctx context.Context, c *conn) {
	defer c.ws.CloseNow()
	for {
		select {
		case <-ctx.Done():
			c.ws.Close(websocket.StatusGoingAway, "")
			return
		case frame := <-c.out:
			wctx, done := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, frame)
			done()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (h *Hub) handshake(ctx context.Context, ws *websocket.Conn, cancel context.CancelFunc) (*conn, error) {
	hctx, done := context.WithTimeout(ctx, helloTimeout)
	defer done()
	_, data, err := ws.Read(hctx)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil || req.Op != "hello" {
		return nil, errors.New("first frame must be hello")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	principal, err := h.store.principalByToken(req.Token)
	if err != nil {
		return nil, err
	}
	if principal == nil {
		return nil, errors.New("unknown or revoked token")
	}
	p := req.Peer
	if p == nil || !validID(p.ID, 200) {
		return nil, errors.New("hello needs peer.id without spaces")
	}
	if p.Machine != principal.Name {
		return nil, fmt.Errorf("this token belongs to machine %q", principal.Name)
	}
	if !strings.HasPrefix(p.ID, p.Machine+"/") {
		return nil, errors.New("peer.id must start with <machine>/")
	}
	if p.Role != "main" && p.Role != "sub" {
		return nil, errors.New("peer.role must be main or sub")
	}
	repo := canonicalRepo(req.Repo)
	if repo == "" {
		return nil, errors.New("hello needs repo (the git origin URL)")
	}
	if !principal.mayJoin(repo) {
		return nil, fmt.Errorf("machine %q has no access to %s", principal.Name, repo)
	}

	now := nowMs()
	c := &conn{
		ws:        ws,
		room:      roomID(repo),
		principal: principal,
		state:     PeerState{PeerInfo: *p, Trust: principal.Role, ConnectedAt: now, LastSeen: now},
		out:       make(chan []byte, sendQueue),
		cancel:    cancel,
	}

	cursor, isNew, err := h.store.cursor(c.room, p.ID)
	if err != nil {
		return nil, err
	}
	var backlog []Message
	if isNew && p.Role == "main" {
		backlog, err = h.store.undeliveredTo(c.room, p.Machine, time.Now().Add(-undeliveredWindow))
	} else {
		backlog, err = h.store.messagesAfter(c.room, cursor)
	}
	if err != nil {
		return nil, err
	}
	claims, err := h.store.claims(c.room)
	if err != nil {
		return nil, err
	}
	room := h.rooms[c.room]
	if room == nil {
		room = map[string]*conn{}
		h.rooms[c.room] = room
	}
	if old := room[p.ID]; old != nil {
		old.cancel() // same session reconnected; newest wins
	}
	room[p.ID] = c

	c.enqueue(mustJSON(obj{
		"id": req.ID, "ok": true, "ev": "welcome",
		"room": c.room, "repo": repo, "trust": principal.Role,
		"peers": h.peersLocked(c.room, c), "claims": claims,
	}))
	for _, m := range backlog {
		if m.From != p.ID && matchTarget(m.To, p.ID) {
			c.enqueue(mustJSON(obj{"ev": "msg", "msg": m}))
		}
	}
	h.broadcastPresenceLocked(c.room)
	log.Printf("room %s: +%s (%s)", c.room, p.ID, principal.Role)
	return c, nil
}

func (h *Hub) disconnect(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[c.room]
	if room[c.state.ID] != c {
		return // replaced by a newer connection
	}
	delete(room, c.state.ID)
	if len(room) == 0 {
		delete(h.rooms, c.room)
	}
	h.broadcastPresenceLocked(c.room)
	log.Printf("room %s: -%s", c.room, c.state.ID)
}

func fail(format string, a ...any) obj {
	return obj{"ok": false, "error": fmt.Sprintf(format, a...)}
}

// allowSendLocked applies the per-principal message rate limit.
func (h *Hub) allowSendLocked(p *Principal) bool {
	cutoff := time.Now().Add(-time.Minute)
	kept := h.sends[p.Name][:0]
	for _, t := range h.sends[p.Name] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= roleLimits[p.Role].sendsPerMinute {
		h.sends[p.Name] = kept
		return false
	}
	h.sends[p.Name] = append(kept, time.Now())
	return true
}

func (h *Hub) handle(c *conn, req *Request) obj {
	h.mu.Lock()
	defer h.mu.Unlock()
	me := c.state.ID
	lim := roleLimits[c.principal.Role]
	c.state.LastSeen = nowMs()

	switch req.Op {
	case "ping":
		if err := h.store.renew(c.room, me); err != nil {
			return fail("renew: %v", err)
		}
		return obj{"ok": true}

	case "status":
		if req.Busy != nil {
			c.state.Busy = *req.Busy
		}
		if req.Task != nil {
			c.state.Task = truncate(*req.Task, 200)
		}
		h.broadcastPresenceLocked(c.room)
		return obj{"ok": true}

	case "peers":
		claims, err := h.store.claims(c.room)
		if err != nil {
			return fail("%v", err)
		}
		return obj{"ok": true, "peers": h.peersLocked(c.room, c), "claims": claims}

	case "send":
		if !validID(req.To, 200) {
			return fail("'to' must be a peer id or pattern without spaces")
		}
		if req.Text == "" || len(req.Text) > maxTextBytes {
			return fail("text must be 1..%d bytes", maxTextBytes)
		}
		if req.Hops > maxHops {
			return fail("hop limit %d reached: this exchange is agents replying to agents; stop and report to your user instead", maxHops)
		}
		if req.Urgent && !lim.urgent {
			return fail("guest machines cannot send urgent messages")
		}
		if !h.allowSendLocked(c.principal) {
			return fail("rate limit: %d messages per minute for machine %s", lim.sendsPerMinute, c.principal.Name)
		}
		var recipients []*conn
		delivered := []string{}
		machines := []string{}
		for id, peer := range h.rooms[c.room] {
			if id != me && matchTarget(req.To, id) {
				recipients = append(recipients, peer)
				delivered = append(delivered, id)
				machines = append(machines, peer.state.Machine)
			}
		}
		m := Message{From: me, FromRole: c.principal.Role, To: req.To, Text: req.Text, Urgent: req.Urgent, Hops: req.Hops}
		if err := h.store.appendMessage(c.room, &m, machines); err != nil {
			return fail("store: %v", err)
		}
		frame := mustJSON(obj{"ev": "msg", "msg": m})
		for _, peer := range recipients {
			peer.enqueue(frame)
		}
		sort.Strings(delivered)
		return obj{"ok": true, "seq": m.Seq, "deliveredNow": delivered}

	case "ack":
		if err := h.store.setCursor(c.room, me, req.Seq); err != nil {
			return fail("%v", err)
		}
		if err := h.store.markDelivered(req.Seq, c.state.Machine); err != nil {
			return fail("%v", err)
		}
		return obj{"ok": true}

	case "claim":
		if len(req.Resources) == 0 || len(req.Resources) > maxClaimBatch {
			return fail("claim 1..%d resources", maxClaimBatch)
		}
		resources := make([]string, 0, len(req.Resources))
		for _, r := range req.Resources {
			n, err := normalizeResource(r)
			if err != nil {
				return fail("%v", err)
			}
			if c.principal.Role == roleGuest {
				// Resource names end up in owners' agent context; keep them paths, not prose.
				if !guestResource.MatchString(n) {
					return fail("guest resources may only use letters, digits and . _ - / * ? : @ +")
				}
				if hasWildcard(n) && literalDir(n) == "" {
					return fail("guest machines cannot claim repo-wide globs like %q", n)
				}
			}
			resources = append(resources, n)
		}
		held, err := h.store.claimCount(c.room, c.state.Machine)
		if err != nil {
			return fail("%v", err)
		}
		if held+len(resources) > lim.maxClaims {
			return fail("claim limit: machine %s may hold %d claims (holds %d)", c.principal.Name, lim.maxClaims, held)
		}
		ttl := req.TTL
		if ttl == 0 {
			ttl = defaultTTL
		}
		ttl = min(max(ttl, minTTL), lim.maxTTL)
		granted, conflicts, err := h.store.claim(c.room, me, resources, ttl)
		if err != nil {
			return fail("%v", err)
		}
		if len(conflicts) > 0 {
			return obj{"ok": false, "error": "conflicting claims", "conflicts": conflicts}
		}
		h.broadcastClaimsLocked(c.room)
		return obj{"ok": true, "claims": granted}

	case "release":
		if req.Force && !lim.forceRelease {
			return fail("guest machines cannot force-release claims")
		}
		resources := make([]string, 0, len(req.Resources))
		for _, r := range req.Resources {
			n, err := normalizeResource(r)
			if err != nil {
				return fail("%v", err)
			}
			resources = append(resources, n)
		}
		released, err := h.store.release(c.room, me, resources, req.Fence, req.Force)
		if err != nil {
			return fail("%v", err)
		}
		if len(released) > 0 {
			h.broadcastLocked(c.room, obj{
				"ev": "released", "by": me, "byRole": c.principal.Role, "claims": released,
				"sha": truncate(req.SHA, 64), "note": truncate(req.Note, 2000), "forced": req.Force,
			})
			h.broadcastClaimsLocked(c.room)
		}
		return obj{"ok": true, "released": released}

	case "ask_release":
		// Live nudge to the sessions holding claims that block the asker. Not
		// stored: a holder that is offline loses its lease on its own.
		if len(req.Resources) == 0 || len(req.Resources) > maxClaimBatch {
			return fail("ask about 1..%d resources", maxClaimBatch)
		}
		wanted := make([]string, 0, len(req.Resources))
		for _, r := range req.Resources {
			n, err := normalizeResource(r)
			if err != nil {
				return fail("%v", err)
			}
			wanted = append(wanted, n)
		}
		if !h.allowSendLocked(c.principal) {
			return fail("rate limit: %d messages per minute for machine %s", lim.sendsPerMinute, c.principal.Name)
		}
		all, err := h.store.claims(c.room)
		if err != nil {
			return fail("%v", err)
		}
		held := map[string][]string{} // asked session -> its family's blocking claims
		offline := []string{}
		for _, cl := range all {
			if sameFamily(cl.Owner, me) || !slices.ContainsFunc(wanted, func(w string) bool { return overlaps(cl.Resource, w) }) {
				continue
			}
			// A finished subagent's claims belong to its family: ask its main agent.
			target := h.rooms[c.room][cl.Owner]
			if target == nil {
				target = h.rooms[c.room][family(cl.Owner)+"/main"]
			}
			if target == nil {
				offline = append(offline, cl.Owner)
				continue
			}
			held[target.state.ID] = append(held[target.state.ID], cl.Resource)
		}
		asked := []string{}
		for id, resources := range held {
			h.rooms[c.room][id].enqueue(mustJSON(obj{"ev": "release_request", "from": me, "fromRole": c.principal.Role, "resources": resources}))
			asked = append(asked, id)
		}
		sort.Strings(asked)
		offline = slices.Compact(slices.Sorted(slices.Values(offline)))
		return obj{"ok": true, "asked": asked, "offline": offline}

	case "board_list":
		items, err := h.store.board(c.room)
		if err != nil {
			return fail("%v", err)
		}
		return obj{"ok": true, "items": items}

	case "board_add":
		title := strings.TrimSpace(req.Title)
		if title == "" || len(title) > 300 || len(req.Notes) > maxTextBytes {
			return fail("title 1..300 bytes, notes up to %d bytes", maxTextBytes)
		}
		item, err := h.store.boardAdd(c.room, c.actor(), title, req.Notes)
		if err != nil {
			return fail("%v", err)
		}
		h.broadcastLocked(c.room, obj{"ev": "board", "by": me, "byRole": c.principal.Role, "item": item})
		return obj{"ok": true, "item": item}

	case "board_update":
		if req.Patch == nil || req.Patch.ID == 0 {
			return fail("patch.id required")
		}
		item, err := h.store.boardUpdate(c.room, c.actor(), *req.Patch)
		if err != nil {
			return fail("%v", err)
		}
		h.broadcastLocked(c.room, obj{"ev": "board", "by": me, "byRole": c.principal.Role, "item": item})
		return obj{"ok": true, "item": item}
	}
	return fail("unknown op %q", req.Op)
}

// janitor expires leases, drops sessions whose credential was revoked or
// changed by the principal CLI, and prunes history.
func (h *Hub) janitor(ctx context.Context) {
	tick := time.NewTicker(janitorInterval)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		h.mu.Lock()
		expired, err := h.store.expireClaims()
		if err != nil {
			log.Printf("expire claims: %v", err)
		}
		for room, claims := range expired {
			h.broadcastLocked(room, obj{"ev": "released", "by": "relay", "claims": claims, "expired": true})
			h.broadcastClaimsLocked(room)
		}
		h.enforcePrincipalsLocked()
		if time.Since(lastPrune) > time.Hour {
			if err := h.store.prune(messageRetention, cursorRetention); err != nil {
				log.Printf("prune: %v", err)
			}
			lastPrune = time.Now()
		}
		h.mu.Unlock()
	}
}

// enforcePrincipalsLocked disconnects sessions whose principal was revoked,
// deleted, demoted, or lost access to the room since they connected.
func (h *Hub) enforcePrincipalsLocked() {
	ps, err := h.store.principals()
	if err != nil {
		log.Printf("load principals: %v", err)
		return
	}
	current := map[string]*Principal{}
	for _, p := range ps {
		current[p.Name] = p
	}
	for _, room := range h.rooms {
		for _, c := range room {
			p := current[c.principal.Name]
			if p == nil || p.Revoked || p.TokenHash != c.principal.TokenHash || p.Role != c.principal.Role || !p.mayJoinRoom(c.room) {
				log.Printf("room %s: dropping %s (credential changed)", c.room, c.state.ID)
				c.cancel()
			}
		}
	}
}
