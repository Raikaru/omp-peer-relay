package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// A principal is one machine's credential. Its name is the only machine name
// it may connect as; its role decides how far the relay and the other
// machines trust what it sends.
type Principal struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"` // roleOwner | roleGuest
	Rooms     []string `json:"rooms"`
	CreatedAt int64    `json:"createdAt"`
	Revoked   bool     `json:"revoked"`
	TokenHash string   `json:"-"`
}

const (
	roleOwner = "owner"
	roleGuest = "guest"
	allRooms  = "*"
)

var machineName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var (
	reScheme = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://`)
	reCreds  = regexp.MustCompile(`^[^@/]+@`)
	rePort   = regexp.MustCompile(`^([^/:]+):\d+/`)
	reScp    = regexp.MustCompile(`^([^/:]+):`)
	reSlash  = regexp.MustCompile(`/{2,}`)
)

// canonicalRepo maps the https, ssh and scp spellings of one remote to one
// string, so every machine cloning the same repo lands in the same room.
func canonicalRepo(url string) string {
	u := strings.ToLower(strings.TrimSpace(url))
	u = reScheme.ReplaceAllString(u, "")
	u = reCreds.ReplaceAllString(u, "")
	u = rePort.ReplaceAllString(u, "$1/")
	u = reScp.ReplaceAllString(u, "$1/")
	u = strings.TrimSuffix(u, ".git")
	u = reSlash.ReplaceAllString(u, "/")
	return strings.TrimRight(u, "/")
}

func roomID(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (p *Principal) mayJoin(canonical string) bool {
	return slices.Contains(p.Rooms, allRooms) || slices.Contains(p.Rooms, canonical)
}

func (p *Principal) mayJoinRoom(id string) bool {
	return slices.Contains(p.Rooms, allRooms) || slices.ContainsFunc(p.Rooms, func(r string) bool { return roomID(r) == id })
}

// ---- store ----

const principalColumns = `name, role, rooms, created_at, revoked, token_hash`

func scanPrincipal(row interface{ Scan(...any) error }) (*Principal, error) {
	var p Principal
	var rooms string
	if err := row.Scan(&p.Name, &p.Role, &rooms, &p.CreatedAt, &p.Revoked, &p.TokenHash); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rooms), &p.Rooms); err != nil {
		return nil, fmt.Errorf("principal %s: bad rooms: %w", p.Name, err)
	}
	return &p, nil
}

// principalByToken returns nil for unknown or revoked tokens.
func (s *Store) principalByToken(token string) (*Principal, error) {
	p, err := scanPrincipal(s.db.QueryRow(`SELECT `+principalColumns+` FROM principals WHERE token_hash = ?`, hashToken(token)))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p.Revoked) {
		return nil, nil
	}
	return p, err
}

func (s *Store) principals() ([]*Principal, error) {
	rows, err := s.db.Query(`SELECT ` + principalColumns + ` FROM principals ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func normalizeRooms(rooms []string) ([]string, error) {
	out := []string{}
	for _, r := range rooms {
		if r == allRooms {
			return []string{allRooms}, nil
		}
		c := canonicalRepo(r)
		if c == "" {
			return nil, fmt.Errorf("empty room %q", r)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// addPrincipal creates a principal and returns its token (shown once).
func (s *Store) addPrincipal(name, role string, rooms []string) (string, error) {
	if !machineName.MatchString(name) {
		return "", fmt.Errorf("name must match %s", machineName)
	}
	if role != roleOwner && role != roleGuest {
		return "", fmt.Errorf("role must be %s or %s", roleOwner, roleGuest)
	}
	rooms, err := normalizeRooms(rooms)
	if err != nil {
		return "", err
	}
	if len(rooms) == 0 {
		return "", errors.New("grant at least one -room (or -all-rooms)")
	}
	if role == roleGuest && slices.Contains(rooms, allRooms) {
		return "", errors.New("guests must be limited to explicit rooms")
	}
	roomsJSON, _ := json.Marshal(rooms)
	token := newToken()
	_, err = s.db.Exec(`INSERT INTO principals(name, token_hash, role, rooms, created_at) VALUES (?,?,?,?,?)`,
		name, hashToken(token), role, string(roomsJSON), nowMs())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return "", fmt.Errorf("principal %s already exists (use rotate or revoke)", name)
	}
	return token, err
}

func (s *Store) updatePrincipal(name, query string, args ...any) error {
	res, err := s.db.Exec(query, append(args, name)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no principal %s", name)
	}
	return nil
}

func (s *Store) rotatePrincipal(name string) (string, error) {
	token := newToken()
	return token, s.updatePrincipal(name, `UPDATE principals SET token_hash = ?, revoked = 0 WHERE name = ?`, hashToken(token))
}

func (s *Store) revokePrincipal(name string) error {
	return s.updatePrincipal(name, `UPDATE principals SET revoked = 1 WHERE name = ?`)
}

func (s *Store) setPrincipalRooms(name string, rooms []string) error {
	rooms, err := normalizeRooms(rooms)
	if err != nil {
		return err
	}
	var role string
	if err := s.db.QueryRow(`SELECT role FROM principals WHERE name = ?`, name).Scan(&role); err != nil {
		return fmt.Errorf("no principal %s", name)
	}
	if len(rooms) == 0 || (role == roleGuest && slices.Contains(rooms, allRooms)) {
		return errors.New("guests need explicit rooms; owners need at least one")
	}
	roomsJSON, _ := json.Marshal(rooms)
	return s.updatePrincipal(name, `UPDATE principals SET rooms = ? WHERE name = ?`, string(roomsJSON))
}

// ---- CLI ----

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const principalUsage = `usage: omp-peer-relay principal <command> [flags]

  add <name> -role owner|guest (-room <repo-url>... | -all-rooms)   create; prints token once
  list                                                              show principals
  rooms <name> (-room <repo-url>... | -all-rooms)                    replace room grants
  rotate <name>                                                     new token (and un-revoke)
  revoke <name>                                                     disable; live sessions are dropped

Common flag: -db <path> (default $RELAY_DB or ./relay.db). Run as the relay's
service user so SQLite side files keep the right owner.`

func principalCLI(args []string) error {
	if len(args) == 0 {
		return errors.New(principalUsage)
	}
	cmd, args := args[0], args[1:]
	var name string
	if cmd != "list" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return errors.New(principalUsage)
		}
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("principal "+cmd, flag.ContinueOnError)
	dbPath := fs.String("db", envOr("RELAY_DB", "relay.db"), "SQLite database path")
	role := fs.String("role", "", "owner | guest")
	all := fs.Bool("all-rooms", false, "grant every room (owners only)")
	var rooms multiFlag
	fs.Var(&rooms, "room", "repo URL to grant (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		rooms = append(rooms, allRooms)
	}
	store, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	switch cmd {
	case "add":
		token, err := store.addPrincipal(name, *role, rooms)
		if err != nil {
			return err
		}
		fmt.Println(token)
	case "rotate":
		token, err := store.rotatePrincipal(name)
		if err != nil {
			return err
		}
		fmt.Println(token)
	case "revoke":
		return store.revokePrincipal(name)
	case "rooms":
		return store.setPrincipalRooms(name, rooms)
	case "list":
		ps, err := store.principals()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tROLE\tSTATUS\tCREATED\tROOMS")
		for _, p := range ps {
			status := "active"
			if p.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Role, status,
				time.UnixMilli(p.CreatedAt).UTC().Format("2006-01-02"), strings.Join(p.Rooms, " "))
		}
		return w.Flush()
	default:
		return errors.New(principalUsage)
	}
	return nil
}
