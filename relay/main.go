// omp-peer-relay coordinates omp agents running on different machines:
// presence, durable mailboxes, leased claims on files/tasks, and a shared
// task board. It never touches code; git carries the code.
//
// Every machine authenticates with its own token, issued by
// `omp-peer-relay principal add`. Owner machines are yours; guest machines
// are limited, and everything they send is stamped untrusted.
//
// Configuration (flags override env):
//
//	RELAY_LISTEN  listen address (default 127.0.0.1:7480)
//	RELAY_DB      SQLite path (default ./relay.db)
//
// Subcommands: `principal` (machine tokens) and `backup -dir D -keep N`
// (consistent snapshot of the live database).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 {
		cli := map[string]func([]string) error{"principal": principalCLI, "backup": backupCLI}[os.Args[1]]
		if cli != nil {
			if err := cli(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			return
		}
	}
	listen := flag.String("listen", envOr("RELAY_LISTEN", "127.0.0.1:7480"), "listen address")
	dbPath := flag.String("db", envOr("RELAY_DB", "relay.db"), "SQLite database path")
	flag.Parse()

	store, err := openStore(*dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer store.Close()
	if ps, err := store.principals(); err != nil {
		log.Fatalf("load principals: %v", err)
	} else if len(ps) == 0 {
		log.Printf("no principals yet; nobody can connect until `omp-peer-relay principal add` issues a token")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := newHub(store)
	go hub.janitor(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /ws", hub.serveWS)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("omp-peer-relay listening on %s (db %s)", *listen, *dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
