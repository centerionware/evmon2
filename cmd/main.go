// cmd/main.go
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/lib/pq" // Postgres driver
	"centerionware.com/evmon/internal"
)

func main() {
	// Database connection (Postgres example)
	dbURL := os.Getenv("EVMON_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://user:pass@localhost:5432/evmon?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	// Initialize store
	store := internal.NewDBStore(db)

	// Initialize controller
	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	// Initial sync of ingresses and CRDs
	if err := controller.SyncIngresses(nil); err != nil {
		log.Printf("warning: failed to sync ingresses: %v", err)
	}
	if err := controller.SyncCRDs(nil); err != nil {
		log.Printf("warning: failed to sync CRDs: %v", err)
	}

	// Initialize prober
	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	// Start HTTP API
	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTP API listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stopCh
	log.Println("shutting down Evmon...")
	server.Close()
}