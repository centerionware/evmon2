package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "modernc.org/sqlite"             // SQLite driver
	_ "github.com/lib/pq"              // Postgres driver
	_ "github.com/go-sql-driver/mysql" // MariaDB/MySQL driver
	"centerionware.com/evmon/internal"
)

func main() {
	dbType := os.Getenv("EVMON_DB_TYPE")       // "sqlite", "postgres", "mariadb"
	dbURL := os.Getenv("EVMON_DATABASE_URL")   // full connection string

	var db *sql.DB
	var err error

	switch dbType {
	case "", "sqlite":
		// Default to SQLite if empty or explicitly sqlite
		if dbURL == "" {
			dbURL = "file:evmon.db?_foreign_keys=on"
		}
		db, err = sql.Open("sqlite", dbURL)
	case "postgres":
		if dbURL == "" {
			log.Fatal("EVMON_DATABASE_URL must be set for Postgres")
		}
		db, err = sql.Open("postgres", dbURL)
	case "mariadb":
		if dbURL == "" {
			log.Fatal("EVMON_DATABASE_URL must be set for MariaDB")
		}
		db, err = sql.Open("mysql", dbURL)
	default:
		log.Fatalf("unsupported EVMON_DB_TYPE: %s", dbType)
	}

	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Initialize store
	store := internal.NewDBStore(db)

	// Initialize controller
	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initial sync
	if err := controller.SyncIngresses(ctx); err != nil {
		log.Printf("warning: failed to sync ingresses: %v", err)
	}
	if err := controller.SyncCRDs(ctx); err != nil {
		log.Printf("warning: failed to sync CRDs: %v", err)
	}

	// Periodic resync
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := controller.SyncIngresses(ctx); err != nil {
					log.Printf("warning: failed to sync ingresses: %v", err)
				}
				if err := controller.SyncCRDs(ctx); err != nil {
					log.Printf("warning: failed to sync CRDs: %v", err)
				}
			}
		}
	}()

	// Start prober
	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	// HTTP API
	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Graceful shutdown
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
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
}