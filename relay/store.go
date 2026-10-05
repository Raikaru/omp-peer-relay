package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists everything that must survive a relay restart: principals,
// the message log with per-machine delivery, per-peer cursors, claims with
// their fencing counter, and the task board. Hub.mu serializes all relay
// calls, so one connection suffices; the principal CLI is a second process
// and relies on SQLite's busy timeout.
type Store struct{ db *sql.DB }

const schemaVersion = 2

const schema = `
CREATE TABLE principals (
	name       TEXT    PRIMARY KEY,
	token_hash TEXT    NOT NULL UNIQUE,
	role       TEXT    NOT NULL,
	rooms      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	room       TEXT    NOT NULL,
	sender     TEXT    NOT NULL,
	from_role  TEXT    NOT NULL,
	target     TEXT    NOT NULL,
	text       TEXT    NOT NULL,
	urgent     INTEGER NOT NULL,
	hops       INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX messages_room_seq ON messages(room, seq);
CREATE TABLE deliveries (
	seq     INTEGER NOT NULL,
	machine TEXT    NOT NULL,
	PRIMARY KEY (seq, machine)
);
CREATE TABLE cursors (
	room       TEXT    NOT NULL,
	peer       TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (room, peer)
);
CREATE TABLE fences (
	room  TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);
CREATE TABLE claims (
	room       TEXT    NOT NULL,
	resource   TEXT    NOT NULL,
	owner      TEXT    NOT NULL,
	fence      INTEGER NOT NULL,
	ttl        INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (room, resource)
);
CREATE TABLE board (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	room              TEXT    NOT NULL,
	title             TEXT    NOT NULL,
	notes             TEXT    NOT NULL DEFAULT '',
	status            TEXT    NOT NULL,
	owner             TEXT    NOT NULL DEFAULT '',
	sha               TEXT    NOT NULL DEFAULT '',
	created_by        TEXT    NOT NULL,
	created_principal TEXT    NOT NULL,
	created_role      TEXT    NOT NULL,
	updated_by        TEXT    NOT NULL,
	updated_role      TEXT    NOT NULL,
	updated_at        INTEGER NOT NULL
);
CREATE INDEX board_room ON board(room, id);
`

// Message.FromRole and the *Role fields below are stamped by the relay from
// the sender's principal, never taken from the client.
type Message struct {
	Seq       int64  `json:"seq"`
	From      string `json:"from"`
	FromRole  string `json:"fromRole"`
	To        string `json:"to"`
	Text      string `json:"text"`
	Urgent    bool   `json:"urgent"`
	Hops      int    `json:"hops"`
	CreatedAt int64  `json:"createdAt"`
}

type Claim struct {
	Resource  string `json:"resource"`
	Owner     string `json:"owner"`
	Fence     int64  `json:"fence"`
	TTL       int    `json:"ttl"`
	ExpiresAt int64  `json:"expiresAt"`
}

type BoardItem struct {
	ID               int64  `json:"id"`
	Title            string `json:"title"`
	Notes            string `json:"notes"`
	Status           string `json:"status"`
	Owner            string `json:"owner"`
	SHA              string `json:"sha"`
	CreatedBy        string `json:"createdBy"`
	CreatedPrincipal string `json:"-"`
	CreatedRole      string `json:"createdRole"`
	UpdatedBy        string `json:"updatedBy"`
	UpdatedRole      string `json:"updatedRole"`
	UpdatedAt        int64  `json:"updatedAt"`
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version, tables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return err
	}
	if tables > 0 {
		return fmt.Errorf("database has schema version %d, this relay needs %d; move the old file aside to start fresh", version, schemaVersion)
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func nowMs() int64 { return time.Now().UnixMilli() }

// ---- messages & cursors ----

func (s *Store) maxSeq() (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM messages`).Scan(&seq)
	return seq.Int64, err
}

// appendMessage stores m and records the machines that received it live.
func (s *Store) appendMessage(room string, m *Message, deliveredTo []string) error {
	m.CreatedAt = nowMs()
	res, err := s.db.Exec(`INSERT INTO messages(room, sender, from_role, target, text, urgent, hops, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		room, m.From, m.FromRole, m.To, m.Text, m.Urgent, m.Hops, m.CreatedAt)
	if err != nil {
		return err
	}
	if m.Seq, err = res.LastInsertId(); err != nil {
		return err
	}
	for _, machine := range deliveredTo {
		if err := s.markDelivered(m.Seq, machine); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markDelivered(seq int64, machine string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO deliveries(seq, machine) VALUES (?, ?)`, seq, machine)
	return err
}

const messageColumns = `seq, sender, from_role, target, text, urgent, hops, created_at`

func (s *Store) queryMessages(query string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.From, &m.FromRole, &m.To, &m.Text, &m.Urgent, &m.Hops, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) messagesAfter(room string, after int64) ([]Message, error) {
	return s.queryMessages(`SELECT `+messageColumns+` FROM messages WHERE room = ? AND seq > ? ORDER BY seq`, room, after)
}

// undeliveredTo lists recent messages that never reached machine. Every omp
// session is a new peer id, so mail for an offline machine waits here for
// that machine's next main agent. Callers still filter by target.
func (s *Store) undeliveredTo(room, machine string, since time.Time) ([]Message, error) {
	return s.queryMessages(`SELECT `+messageColumns+` FROM messages m
		WHERE room = ? AND created_at >= ? AND sender NOT LIKE ? ESCAPE '\'
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.seq = m.seq AND d.machine = ?)
		ORDER BY seq`, room, since.UnixMilli(), likePrefix(machine+"/"), machine)
}

// likePrefix escapes s for a LIKE prefix match.
func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// cursor returns the peer's last acknowledged seq. A peer seen for the first
// time starts at the current head (isNew) so it never replays old traffic.
func (s *Store) cursor(room, peer string) (seq int64, isNew bool, err error) {
	err = s.db.QueryRow(`SELECT seq FROM cursors WHERE room = ? AND peer = ?`, room, peer).Scan(&seq)
	if err == nil {
		return seq, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if seq, err = s.maxSeq(); err != nil {
		return 0, false, err
	}
	return seq, true, s.setCursor(room, peer, seq)
}

func (s *Store) setCursor(room, peer string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO cursors(room, peer, seq, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(room, peer) DO UPDATE SET seq = MAX(seq, excluded.seq), updated_at = excluded.updated_at`,
		room, peer, seq, nowMs())
	return err
}

// ---- claims ----

func (s *Store) claims(room string) ([]Claim, error) {
	rows, err := s.db.Query(`SELECT resource, owner, fence, ttl, expires_at FROM claims WHERE room = ? ORDER BY resource`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Claim{}
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Resource, &c.Owner, &c.Fence, &c.TTL, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) nextFence(tx *sql.Tx, room string) (int64, error) {
	var v int64
	err := tx.QueryRow(`INSERT INTO fences(room, value) VALUES (?, 1)
		ON CONFLICT(room) DO UPDATE SET value = value + 1 RETURNING value`, room).Scan(&v)
	return v, err
}

// claim grants every resource or none. Existing claims by the same owner on
// the same resource are renewed and keep their fence.
func (s *Store) claim(room, owner string, resources []string, ttl int) (granted []Claim, conflicts []Claim, err error) {
	existing, err := s.claims(room)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range resources {
		for _, c := range existing {
			if !sameFamily(c.Owner, owner) && overlaps(r, c.Resource) {
				conflicts = append(conflicts, c)
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	expires := nowMs() + int64(ttl)*1000
	for _, r := range resources {
		c := Claim{Resource: r, Owner: owner, TTL: ttl, ExpiresAt: expires}
		// An exact re-claim within the family renews the lease and keeps
		// the original holder and fence.
		err := tx.QueryRow(`SELECT owner, fence FROM claims WHERE room = ? AND resource = ?`, room, r).Scan(&c.Owner, &c.Fence)
		switch {
		case err == nil:
			_, err = tx.Exec(`UPDATE claims SET ttl = ?, expires_at = ? WHERE room = ? AND resource = ?`, ttl, expires, room, r)
		case errors.Is(err, sql.ErrNoRows):
			if c.Fence, err = s.nextFence(tx, room); err == nil {
				_, err = tx.Exec(`INSERT INTO claims(room, resource, owner, fence, ttl, expires_at) VALUES (?,?,?,?,?,?)`,
					room, r, owner, c.Fence, ttl, expires)
			}
		}
		if err != nil {
			return nil, nil, err
		}
		granted = append(granted, c)
	}
	return granted, nil, tx.Commit()
}

// renew extends every lease held by owner by its own TTL.
func (s *Store) renew(room, owner string) error {
	_, err := s.db.Exec(`UPDATE claims SET expires_at = ? + ttl * 1000 WHERE room = ? AND owner = ?`, nowMs(), room, owner)
	return err
}

// release removes the named claims (all of owner's when resources is empty).
// Without force only the owner may release; a non-zero fence must match.
func (s *Store) release(room, owner string, resources []string, fence int64, force bool) ([]Claim, error) {
	all, err := s.claims(room)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, r := range resources {
		want[r] = true
	}
	var targets []Claim
	for _, c := range all {
		if len(want) == 0 {
			if c.Owner == owner {
				targets = append(targets, c)
			}
			continue
		}
		if !want[c.Resource] {
			continue
		}
		delete(want, c.Resource)
		if !force && !sameFamily(c.Owner, owner) {
			return nil, fmt.Errorf("%s is held by %s; only its owner's session may release it (or use force)", c.Resource, c.Owner)
		}
		if fence != 0 && c.Fence != fence {
			return nil, fmt.Errorf("stale fence for %s: have %d, current %d (lease was lost and re-granted)", c.Resource, fence, c.Fence)
		}
		targets = append(targets, c)
	}
	for r := range want {
		return nil, fmt.Errorf("no claim on %s", r)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, c := range targets {
		if _, err := tx.Exec(`DELETE FROM claims WHERE room = ? AND resource = ? AND fence = ?`, room, c.Resource, c.Fence); err != nil {
			return nil, err
		}
	}
	return targets, tx.Commit()
}

// expireClaims deletes lapsed leases and returns them grouped by room.
func (s *Store) expireClaims() (map[string][]Claim, error) {
	rows, err := s.db.Query(`DELETE FROM claims WHERE expires_at < ? RETURNING room, resource, owner, fence, ttl, expires_at`, nowMs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Claim{}
	for rows.Next() {
		var room string
		var c Claim
		if err := rows.Scan(&room, &c.Resource, &c.Owner, &c.Fence, &c.TTL, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out[room] = append(out[room], c)
	}
	return out, rows.Err()
}

// claimCount counts active claims held by any peer of machine.
func (s *Store) claimCount(room, machine string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM claims WHERE room = ? AND owner LIKE ? ESCAPE '\'`, room, likePrefix(machine+"/")).Scan(&n)
	return n, err
}

// prune drops old messages (with their delivery records) and cursors of
// peers gone for a long time.
func (s *Store) prune(msgAge, cursorAge time.Duration) error {
	now := time.Now()
	if _, err := s.db.Exec(`DELETE FROM messages WHERE created_at < ?`, now.Add(-msgAge).UnixMilli()); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM deliveries WHERE seq NOT IN (SELECT seq FROM messages)`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM cursors WHERE updated_at < ?`, now.Add(-cursorAge).UnixMilli())
	return err
}

// ---- board ----

// Actor is the authenticated sender of a write: the peer and the principal
// (machine credential) it connected with.
type Actor struct {
	Peer      string
	Principal string
	Role      string
}

var boardStatuses = map[string]bool{"open": true, "claimed": true, "done": true, "blocked": true}

const boardColumns = `id, title, notes, status, owner, sha, created_by, created_principal, created_role, updated_by, updated_role, updated_at`

func scanBoardItem(row interface{ Scan(...any) error }) (BoardItem, error) {
	var b BoardItem
	err := row.Scan(&b.ID, &b.Title, &b.Notes, &b.Status, &b.Owner, &b.SHA, &b.CreatedBy, &b.CreatedPrincipal, &b.CreatedRole, &b.UpdatedBy, &b.UpdatedRole, &b.UpdatedAt)
	return b, err
}

func (s *Store) board(room string) ([]BoardItem, error) {
	rows, err := s.db.Query(`SELECT `+boardColumns+` FROM board WHERE room = ? ORDER BY id`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoardItem{}
	for rows.Next() {
		b, err := scanBoardItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) boardItem(room string, id int64) (BoardItem, error) {
	b, err := scanBoardItem(s.db.QueryRow(`SELECT `+boardColumns+` FROM board WHERE room = ? AND id = ?`, room, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("no board item %d", id)
	}
	return b, err
}

func (s *Store) boardAdd(room string, by Actor, title, notes string) (BoardItem, error) {
	res, err := s.db.Exec(`INSERT INTO board(room, title, notes, status, created_by, created_principal, created_role, updated_by, updated_role, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		room, title, notes, "open", by.Peer, by.Principal, by.Role, by.Peer, by.Role, nowMs())
	if err != nil {
		return BoardItem{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return BoardItem{}, err
	}
	return s.boardItem(room, id)
}

// BoardPatch fields left nil are unchanged.
type BoardPatch struct {
	ID     int64   `json:"id"`
	Title  *string `json:"title,omitempty"`
	Notes  *string `json:"notes,omitempty"`
	Status *string `json:"status,omitempty"`
	Owner  *string `json:"owner,omitempty"`
	SHA    *string `json:"sha,omitempty"`
}

// boardUpdate applies p. Guests may only touch items they created or that
// are assigned to one of their peers, and may only assign to themselves.
func (s *Store) boardUpdate(room string, by Actor, p BoardPatch) (BoardItem, error) {
	b, err := s.boardItem(room, p.ID)
	if err != nil {
		return b, err
	}
	if by.Role == roleGuest {
		mine := by.Principal + "/"
		if b.CreatedPrincipal != by.Principal && !strings.HasPrefix(b.Owner, mine) {
			return b, fmt.Errorf("guests may only update board items they created or own")
		}
		if p.Owner != nil && *p.Owner != "" && !strings.HasPrefix(*p.Owner, mine) {
			return b, fmt.Errorf("guests may only assign items to their own peers (%s*)", mine)
		}
	}
	if p.Title != nil {
		b.Title = *p.Title
	}
	if p.Notes != nil {
		b.Notes = *p.Notes
	}
	if p.Status != nil {
		if !boardStatuses[*p.Status] {
			return b, fmt.Errorf("invalid status %q (open|claimed|done|blocked)", *p.Status)
		}
		b.Status = *p.Status
	}
	if p.Owner != nil {
		b.Owner = *p.Owner
	}
	if p.SHA != nil {
		b.SHA = *p.SHA
	}
	_, err = s.db.Exec(`UPDATE board SET title = ?, notes = ?, status = ?, owner = ?, sha = ?, updated_by = ?, updated_role = ?, updated_at = ? WHERE room = ? AND id = ?`,
		b.Title, b.Notes, b.Status, b.Owner, b.SHA, by.Peer, by.Role, nowMs(), room, b.ID)
	if err != nil {
		return b, err
	}
	return s.boardItem(room, b.ID)
}
