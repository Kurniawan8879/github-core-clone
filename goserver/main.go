// main.go — the Go forge entrypoint.
//
//  1. Port comes from argv[1] (or $PORT); "0" lets the kernel pick one,
//     which main.ts reads back from the "[go] listening on <port>" line.
//  2. NewStore + NewHandler, listen, print the listening line to stdout,
//     then serve in the background so /healthz answers while warm-up runs.
//  3. WarmUpForge(port) unless MAKEFUN_WARMUP_OFF=1 (best-effort, bounded).
//  4. Serve until SIGTERM (or SIGINT), then shut down and exit 0.
//
// No logging to stdout besides the listening line (stderr is fine).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := ""
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "[go] no port: pass argv[1] or set PORT")
		os.Exit(1)
	}

	store := NewStore()
	handler := NewHandler(store)

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[go] listen: %v\n", err)
		os.Exit(1)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// Single line on stdout: main.ts parses the real port from this.
	fmt.Printf("[go] listening on %d\n", actualPort)

	if os.Getenv("MAKEFUN_WARMUP_OFF") != "1" {
		WarmUpForge(actualPort)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sigCh:
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[go] serve: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = srv.Close()
	os.Exit(0)
}
