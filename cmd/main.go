// cmd/main.go
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

	_ "modernc.org/sqlite"
	_ "github.com/lib/pq"
	_ "github.com/go-sql-driver/mysql"
	"centerionware.com/evmon/internal"
)

func main() {
	dbType := os.Getenv("EVMON_DB_TYPE")
	dbURL := os.Getenv("EVMON_DATABASE_URL")

	var db *sql.DB
	var err error

	switch dbType {
	case "", "sqlite":
		if dbURL == "" {
			dbURL = "file:/data/evmon.db?mode=rwc&_foreign_keys=on&cache=shared"
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

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}

	store := internal.NewDBStore(db, dbType)

	if err := store.Migrate(); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}
	log.Println("database migration completed")

	defer db.Close()

	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := controller.SyncIngresses(ctx); err != nil {
		log.Printf("warning: failed to sync ingresses: %v", err)
	}
	if err := controller.SyncCRDs(ctx); err != nil {
		log.Printf("warning: failed to sync CRDs: %v", err)
	}

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

	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

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