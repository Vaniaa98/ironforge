package main

// Forge entrypoint: binds an ephemeral port, registers it with the env,
// serves all four surfaces from the in-memory store, polls /done, exits 0.
// Deterministic: no wall clock, no randomness, no UUIDs in any response;
// every timestamp is the fixed fixture epoch.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	requestTimeout = 30 * time.Second
	retryInitial   = 250 * time.Millisecond
	retryMax       = 2 * time.Second
)

func sleep(d time.Duration) { time.Sleep(d) }

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, "POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, "GET", envURL+"/done", nil)
		if err == nil {
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				var body struct {
					Done bool `json:"done"`
				}
				_ = json.NewDecoder(res.Body).Decode(&body)
				_ = res.Body.Close()
				if res.StatusCode == 200 && body.Done {
					cancel()
					return
				}
			}
		}
		cancel()
		sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	store := NewStore(token)

	mux := http.NewServeMux()
	registerREST(mux, store)
	registerGraphQL(mux, store)
	registerMCP(mux, store)
	// Git smart HTTP is dispatched here, not as ServeMux patterns: the
	// patterns GET /{owner}/{repo}/info/refs etc. conflict with the REST
	// /api/v3/{path...} fallback (both match /api/v3/info/refs) and the
	// mux panics at registration. The "/" catch-all never conflicts.
	// Health probe for the env's boot check (wire.ts HEALTHZ_PATH): GET
	// /healthz must answer 200, unauthenticated, twice inside 10 s, or
	// the trial's working bit stays 0. Only the status matters.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, store)
			return
		}
		writeJSON(w, store, 200, struct {
			OK bool `json:"ok"`
		}{OK: true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if gitDispatch(w, r, store) {
			return
		}
		notFoundJSON(w, store)
	})

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", addr.Port)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT (the launcher forwards them).
	quit := make(chan os.Signal, 2)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	go func() {
		_ = srv.Serve(ln)
	}()

	if err := register(envURL, addr.Port); err != nil {
		fmt.Fprintln(os.Stderr, "[forge] register:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[forge] registered; waiting for done")
	waitForDone(envURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fmt.Fprintln(os.Stderr, "[forge] done")
}
