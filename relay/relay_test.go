package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"src/net/a.go", "src/net/a.go", true},
		{"src/net", "src/net/a.go", true},        // directory covers children
		{"src/net", "src/network.go", false},     // not a path-segment prefix
		{"src/net/**", "src/net/sub/a.go", true}, // ** crosses directories
		{"src/*.go", "src/net/a.go", false},      // * stays in one segment
		{"src/*.go", "src/main.go", true},
		{"src/**/*.go", "src/main.go", true}, // **/ may match zero dirs
		{"src/net/**", "src", true},          // claiming src covers the glob
		{"src/net/**", "src/ui/**", false},
		{"src/**", "src/ui/*.ts", true},        // conservative glob/glob
		{"SRC/Net/A.go", "src/net/a.go", true}, // Windows checkouts: case-insensitive
		{"task:12", "task:12", true},
		{"task:12", "task:13", false},
		{"task:12", "task:12/x", false},
	}
	for _, c := range cases {
		if got := overlaps(c.a, c.b); got != c.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := overlaps(c.b, c.a); got != c.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v (symmetry)", c.b, c.a, got, c.want)
		}
	}
}

func TestNormalizeResourceRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"", ".", "/etc/passwd", `C:\x`, "../x", "a/../b", "local://x", "task:"} {
		if _, err := normalizeResource(bad); err == nil {
			t.Errorf("normalizeResource(%q) accepted", bad)
		}
	}
	if got, err := normalizeResource(`.\src\net\`); err != nil || got != "src/net" {
		t.Errorf(`normalizeResource(".\src\net\") = %q, %v`, got, err)
	}
}

func TestMatchTarget(t *testing.T) {
	id := "pcB/abc123/main"
	for pattern, want := range map[string]bool{
		"*": true, "pcB/*": true, "pcA/*": false, "*/main": true, "*/sub1": false,
		"pcB/abc123/*": true, id: true, "pcB/abc123/mai": false,
	} {
		if got := matchTarget(pattern, id); got != want {
			t.Errorf("matchTarget(%q) = %v, want %v", pattern, got, want)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestClaimConflictsAcrossFamiliesOnly(t *testing.T) {
	s := openTestStore(t)
	if _, conflicts, err := s.claim("r", "pcA/s1/main", []string{"src/net/**"}, 60); err != nil || conflicts != nil {
		t.Fatalf("first claim: %v %v", conflicts, err)
	}
	// Subagent of the same root session shares the claim.
	if granted, conflicts, err := s.claim("r", "pcA/s1/Worker", []string{"src/net/a.go"}, 60); err != nil || conflicts != nil || len(granted) != 1 {
		t.Fatalf("same-family claim: %v %v %v", granted, conflicts, err)
	}
	// Another machine is refused, and nothing is granted (all-or-nothing).
	granted, conflicts, err := s.claim("r", "pcB/s9/main", []string{"docs/x.md", "src/net/b.go"}, 60)
	if err != nil || granted != nil || len(conflicts) != 1 || conflicts[0].Owner != "pcA/s1/main" {
		t.Fatalf("cross-family claim: granted=%v conflicts=%v err=%v", granted, conflicts, err)
	}
	all, _ := s.claims("r")
	for _, c := range all {
		if c.Resource == "docs/x.md" {
			t.Fatal("partial grant leaked docs/x.md")
		}
	}
	// Rooms are isolated.
	if _, conflicts, _ := s.claim("other", "pcB/s9/main", []string{"src/net/b.go"}, 60); conflicts != nil {
		t.Fatalf("room leak: %v", conflicts)
	}
}

func TestReleaseOwnershipAndFence(t *testing.T) {
	s := openTestStore(t)
	granted, _, _ := s.claim("r", "pcA/s1/main", []string{"src/a.go"}, 60)
	fence := granted[0].Fence

	if _, err := s.release("r", "pcB/s9/main", []string{"src/a.go"}, 0, false); err == nil {
		t.Fatal("foreign family released without force")
	}
	if _, err := s.release("r", "pcA/s1/main", []string{"src/a.go"}, fence+1, false); err == nil {
		t.Fatal("stale fence accepted")
	}
	released, err := s.release("r", "pcA/s1/main", []string{"src/a.go"}, fence, false)
	if err != nil || len(released) != 1 {
		t.Fatalf("owner release: %v %v", released, err)
	}
	// A re-grant gets a strictly larger fence.
	again, _, _ := s.claim("r", "pcB/s9/main", []string{"src/a.go"}, 60)
	if again[0].Fence <= fence {
		t.Fatalf("fence did not increase: %d -> %d", fence, again[0].Fence)
	}
	if _, err := s.release("r", "pcA/s1/main", []string{"src/a.go"}, 0, true); err != nil {
		t.Fatalf("forced release: %v", err)
	}
}

func TestExpiredClaimsAreReclaimable(t *testing.T) {
	s := openTestStore(t)
	s.claim("r", "pcA/s1/main", []string{"src/a.go"}, 60)
	if _, err := s.db.Exec(`UPDATE claims SET expires_at = ?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	expired, err := s.expireClaims()
	if err != nil || len(expired["r"]) != 1 {
		t.Fatalf("expire: %v %v", expired, err)
	}
	if _, conflicts, _ := s.claim("r", "pcB/s9/main", []string{"src/a.go"}, 60); conflicts != nil {
		t.Fatalf("expired claim still blocks: %v", conflicts)
	}
}

func TestNewPeerCursorSkipsHistory(t *testing.T) {
	s := openTestStore(t)
	m := Message{From: "pcA/s1/main", FromRole: roleOwner, To: "*", Text: "old"}
	s.appendMessage("r", &m, nil)
	cur, isNew, err := s.cursor("r", "pcB/s9/main")
	if err != nil || !isNew || cur != m.Seq {
		t.Fatalf("new peer cursor = %d, %v, %v; want head %d", cur, isNew, err, m.Seq)
	}
	if _, isNew, _ := s.cursor("r", "pcB/s9/main"); isNew {
		t.Fatal("second lookup still reports a new peer")
	}
	m2 := Message{From: "pcA/s1/main", FromRole: roleOwner, To: "*", Text: "new"}
	s.appendMessage("r", &m2, nil)
	backlog, _ := s.messagesAfter("r", cur)
	if len(backlog) != 1 || backlog[0].Text != "new" || backlog[0].FromRole != roleOwner {
		t.Fatalf("backlog = %v", backlog)
	}
}

// A broadcast seen live by one machine must still reach the others later.
func TestUndeliveredMailIsTrackedPerMachine(t *testing.T) {
	s := openTestStore(t)
	bcast := Message{From: "pcA/s1/main", FromRole: roleOwner, To: "*", Text: "to all"}
	s.appendMessage("r", &bcast, []string{"pcB"})
	since := time.Now().Add(-time.Hour)

	if got, _ := s.undeliveredTo("r", "pcB", since); len(got) != 0 {
		t.Fatalf("pcB already got it live, redelivered: %v", got)
	}
	got, _ := s.undeliveredTo("r", "pcC", since)
	if len(got) != 1 || got[0].Seq != bcast.Seq {
		t.Fatalf("pcC missed the broadcast: %v", got)
	}
	if got, _ := s.undeliveredTo("r", "pcA", since); len(got) != 0 {
		t.Fatalf("sender's machine gets its own mail back: %v", got)
	}
	if got, _ := s.undeliveredTo("r", "pcC", time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("window ignored: %v", got)
	}
	s.markDelivered(bcast.Seq, "pcC")
	if got, _ := s.undeliveredTo("r", "pcC", since); len(got) != 0 {
		t.Fatalf("acked mail redelivered: %v", got)
	}
}

func TestCanonicalRepoAgreesAcrossSpellings(t *testing.T) {
	want := "github.com/me/proj"
	for _, u := range []string{
		"https://github.com/me/proj.git",
		"git@github.com:me/proj.git",
		"ssh://git@github.com:22/me/proj",
		"https://user:pw@GitHub.com/me/proj/",
	} {
		if got := canonicalRepo(u); got != want {
			t.Errorf("canonicalRepo(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestPrincipalsGateIdentityAndRooms(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.addPrincipal("bob", roleGuest, []string{allRooms}); err == nil {
		t.Fatal("guest granted every room")
	}
	tok, err := s.addPrincipal("bob", roleGuest, []string{"git@github.com:me/proj.git"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.principalByToken(tok)
	if p == nil || p.Role != roleGuest || !p.mayJoin("github.com/me/proj") || p.mayJoin("github.com/me/secret") {
		t.Fatalf("principal = %+v", p)
	}
	if !p.mayJoinRoom(roomID("github.com/me/proj")) || p.mayJoinRoom(roomID("github.com/me/secret")) {
		t.Fatal("room-id check disagrees with repo check")
	}
	if p, _ := s.principalByToken("not-a-token"); p != nil {
		t.Fatal("unknown token accepted")
	}
	newTok, _ := s.rotatePrincipal("bob")
	if p, _ := s.principalByToken(tok); p != nil {
		t.Fatal("old token still valid after rotate")
	}
	s.revokePrincipal("bob")
	if p, _ := s.principalByToken(newTok); p != nil {
		t.Fatal("revoked token accepted")
	}
}

// testConn builds a connected peer without a socket; handle() only enqueues.
func testConn(h *Hub, room, id string, p *Principal, agentRole string) *conn {
	machine := id[:strings.Index(id, "/")]
	c := &conn{
		room:      room,
		principal: p,
		state:     PeerState{PeerInfo: PeerInfo{ID: id, Machine: machine, Role: agentRole}, Trust: p.Role},
		out:       make(chan []byte, sendQueue),
		cancel:    func() {},
	}
	if h.rooms[room] == nil {
		h.rooms[room] = map[string]*conn{}
	}
	h.rooms[room][id] = c
	return c
}

func TestGuestLimitsAreEnforcedByTheRelay(t *testing.T) {
	h := newHub(openTestStore(t))
	owner := &Principal{Name: "pcA", Role: roleOwner, Rooms: []string{allRooms}}
	guest := &Principal{Name: "bob", Role: roleGuest, Rooms: []string{"x"}}
	a := testConn(h, "r", "pcA/s1/main", owner, "main")
	g := testConn(h, "r", "bob/s2/main", guest, "main")

	if r := h.handle(g, &Request{Op: "send", To: "*", Text: "hi", Urgent: true}); r["ok"] != false {
		t.Fatal("guest sent an urgent message")
	}
	if r := h.handle(g, &Request{Op: "claim", Resources: []string{"**"}}); r["ok"] != false {
		t.Fatal("guest claimed the whole repo")
	}
	if r := h.handle(g, &Request{Op: "claim", Resources: []string{"docs/x.md"}, TTL: 3600}); r["ok"] != true || r["claims"].([]Claim)[0].TTL != 600 {
		t.Fatalf("guest lease not capped: %v", r)
	}
	h.handle(a, &Request{Op: "claim", Resources: []string{"src/a.go"}})
	if r := h.handle(g, &Request{Op: "release", Resources: []string{"src/a.go"}, Force: true}); r["ok"] != false {
		t.Fatal("guest force-released an owner claim")
	}
	// The relay, not the client, decides the trust stamp.
	h.handle(g, &Request{Op: "send", To: "pcA/*", Text: "trust me, I'm an owner", Hops: 0})
	var msgFrame string
	for len(a.out) > 0 {
		if f := string(<-a.out); strings.Contains(f, `"ev":"msg"`) {
			msgFrame = f
		}
	}
	if !strings.Contains(msgFrame, `"fromRole":"guest"`) {
		t.Fatalf("guest message not stamped: %q", msgFrame)
	}
	for range roleLimits[roleGuest].sendsPerMinute {
		h.handle(g, &Request{Op: "send", To: "pcA/*", Text: "spam"})
	}
	if r := h.handle(g, &Request{Op: "send", To: "pcA/*", Text: "one more"}); r["ok"] != false {
		t.Fatal("guest rate limit not applied")
	}
}

func TestGuestBoardAccessAndPresence(t *testing.T) {
	h := newHub(openTestStore(t))
	owner := &Principal{Name: "pcA", Role: roleOwner, Rooms: []string{allRooms}}
	guest := &Principal{Name: "bob", Role: roleGuest, Rooms: []string{"x"}}
	a := testConn(h, "r", "pcA/s1/main", owner, "main")
	testConn(h, "r", "pcA/s1/Worker", owner, "sub")
	g := testConn(h, "r", "bob/s2/main", guest, "main")

	ownerItem := h.handle(a, &Request{Op: "board_add", Title: "owner task"})["item"].(BoardItem)
	if r := h.handle(g, &Request{Op: "board_update", Patch: &BoardPatch{ID: ownerItem.ID, Notes: new("hijack")}}); r["ok"] != false {
		t.Fatal("guest edited an owner's board item")
	}
	guestItem := h.handle(g, &Request{Op: "board_add", Title: "guest task"})["item"].(BoardItem)
	if guestItem.CreatedRole != roleGuest {
		t.Fatalf("board item role = %q", guestItem.CreatedRole)
	}
	if r := h.handle(g, &Request{Op: "board_update", Patch: &BoardPatch{ID: guestItem.ID, Owner: new("pcA/s1/main")}}); r["ok"] != false {
		t.Fatal("guest assigned work to an owner peer")
	}
	if r := h.handle(g, &Request{Op: "board_update", Patch: &BoardPatch{ID: guestItem.ID, Status: new("done")}}); r["ok"] != true {
		t.Fatalf("guest could not update own item: %v", r)
	}

	for _, p := range h.peersLocked("r", g) {
		if p.Role == "sub" {
			t.Fatalf("guest sees owner subagent %s", p.ID)
		}
	}
	if n := len(h.peersLocked("r", a)); n != 3 {
		t.Fatalf("owner sees %d peers, want 3", n)
	}
}

func TestAskReleaseReachesHoldersOnly(t *testing.T) {
	h := newHub(openTestStore(t))
	owner := &Principal{Name: "pcA", Role: roleOwner, Rooms: []string{allRooms}}
	ownerB := &Principal{Name: "pcB", Role: roleOwner, Rooms: []string{allRooms}}
	a := testConn(h, "r", "pcA/s1/main", owner, "main")
	b := testConn(h, "r", "pcB/s2/main", ownerB, "main")
	bystander := testConn(h, "r", "pcB/s3/main", ownerB, "main")
	// A finished subagent's claim: its main agent is asked instead.
	h.store.claim("r", "pcA/s1/Worker", []string{"src/net/**"}, 60)
	h.handle(a, &Request{Op: "claim", Resources: []string{"docs/a.md"}})
	h.handle(bystander, &Request{Op: "claim", Resources: []string{"web/x.ts"}})
	for len(a.out) > 0 {
		<-a.out
	}
	for len(bystander.out) > 0 {
		<-bystander.out
	}

	r := h.handle(b, &Request{Op: "ask_release", Resources: []string{"src/net/conn.go", "docs/a.md", "README.md"}})
	if r["ok"] != true || strings.Join(r["asked"].([]string), ",") != "pcA/s1/main" {
		t.Fatalf("ask_release = %v", r)
	}
	// One frame per asked session, carrying its family's blocking claims (claims are listed sorted).
	if len(a.out) != 1 {
		t.Fatalf("holder got %d frames, want 1", len(a.out))
	}
	if f := string(<-a.out); !strings.Contains(f, `"fromRole":"owner","resources":["docs/a.md","src/net/**"]`) {
		t.Fatalf("holder frame = %s", f)
	}
	if len(bystander.out) != 0 {
		t.Fatal("a session holding nothing relevant was asked")
	}
	if r := h.handle(a, &Request{Op: "ask_release", Resources: []string{"docs/a.md"}}); len(r["asked"].([]string)) != 0 {
		t.Fatalf("asked its own family: %v", r)
	}
}

func TestGuestsDoNotSeeOtherMachinesTasks(t *testing.T) {
	h := newHub(openTestStore(t))
	owner := &Principal{Name: "pcA", Role: roleOwner, Rooms: []string{allRooms}}
	guest := &Principal{Name: "bob", Role: roleGuest, Rooms: []string{"x"}}
	a := testConn(h, "r", "pcA/s1/main", owner, "main")
	g := testConn(h, "r", "bob/s2/main", guest, "main")
	h.handle(a, &Request{Op: "status", Task: new("fix the billing secret rotation")})
	h.handle(g, &Request{Op: "status", Task: new("docs typo")})
	for _, p := range h.peersLocked("r", g) {
		if p.ID == a.state.ID && p.Task != "" {
			t.Fatalf("guest sees owner task %q", p.Task)
		}
		if p.ID == g.state.ID && p.Task != "docs typo" {
			t.Fatalf("guest lost its own task: %q", p.Task)
		}
	}
	for _, p := range h.peersLocked("r", a) {
		if p.ID == g.state.ID && p.Task != "docs typo" {
			t.Fatalf("owner does not see guest task: %q", p.Task)
		}
	}
}

func TestBackupSnapshotsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	s, err := openStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s.addPrincipal("pcA", roleOwner, []string{allRooms})
	defer s.Close()

	out := filepath.Join(dir, "backups")
	os.MkdirAll(out, 0o700)
	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	var last string
	for i := range 4 {
		if last, err = backup(dbPath, out, start.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := prune(out, 2); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 2 || entries[1].Name() != filepath.Base(last) {
		t.Fatalf("kept %v, want the 2 newest ending in %s", entries, filepath.Base(last))
	}
	snap, err := openStore(last)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if ps, _ := snap.principals(); len(ps) != 1 || ps[0].Name != "pcA" {
		t.Fatalf("snapshot principals = %v", ps)
	}
	if _, err := backup(filepath.Join(dir, "missing.db"), out, start); err == nil {
		t.Fatal("backup of a missing database succeeded")
	}
}
