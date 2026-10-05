package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// backupCLI writes a consistent snapshot of the live database (VACUUM INTO
// is safe while the relay runs) and keeps the newest `keep` snapshots.
func backupCLI(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("RELAY_DB", "relay.db"), "SQLite database path")
	dir := fs.String("dir", "backups", "directory for snapshots")
	keep := fs.Int("keep", 14, "snapshots to keep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keep < 1 {
		return fmt.Errorf("-keep must be at least 1")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	path, err := backup(*dbPath, *dir, time.Now())
	if err != nil {
		return err
	}
	fmt.Println(path)
	return prune(*dir, *keep)
}

func backup(dbPath, dir string, now time.Time) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", err // sql.Open would silently create an empty database
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	dest := filepath.Join(dir, "relay-"+now.UTC().Format("20060102-150405")+".db")
	tmp := dest + ".partial"
	os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("snapshot: %w", err)
	}
	// Snapshots hold token hashes: owner-only, like the database.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	return dest, os.Rename(tmp, dest)
}

func prune(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var snaps []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "relay-") && strings.HasSuffix(n, ".db") {
			snaps = append(snaps, n)
		}
	}
	slices.Sort(snaps) // timestamped names sort chronologically
	for _, n := range snaps[:max(0, len(snaps)-keep)] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}
