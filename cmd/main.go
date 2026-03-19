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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/dynamic/dynamicinformer"
)

func main() {
	dbType := os.Getenv("EVMON_DB_TYPE")
	dbURL := os.Getenv("EVMON_DATABASE_URL")

	var db *sql.DB
	var err error

	switch dbType {
	case "", "sqlite":
		if dbURL == "" {
			dbURL = "file:/data/evmon.db?_foreign_keys=on&cache=shared"
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
	defer db.Close()

	store := internal.NewDBStore(db, dbType)
	if err := store.Migrate(); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	// Kubernetes config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create clientset: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create dynamic client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Ingress Informer ---
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			endpoints := controller.ExtractIngress(ing)

			for _, ep := range endpoints {
				if err := store.UpsertEndpoint(ep); err != nil {
					log.Printf("failed to upsert ingress endpoint: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			serviceIDs := controller.ExtractIngressServiceIDs(ing)

			for _, id := range serviceIDs {
				if err := store.DeleteServiceID(id); err != nil {
					log.Printf("failed to delete ingress serviceID: %v", err)
				}
			}
		},
	})

	// --- CRD Informer ---
	evmonGVR := schema.GroupVersionResource{
		Group:    "evmon.centerionware.com",
		Version:  "v1",
		Resource: "evmonendpoints",
	}

	crdInformer := dynamicinformer.NewFilteredDynamicInformer(
		dynClient,
		evmonGVR,
		metav1.NamespaceAll,
		0,
		cache.Indexers{},
		nil,
	)

	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)

			ep := controller.ExtractCRD(u)

			if err := store.UpsertEndpoint(ep); err != nil {
				log.Printf("failed to upsert CRD: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)

			serviceID, _, _ := unstructured.NestedString(u.Object, "spec", "serviceID")

			if err := store.DeleteServiceID(serviceID); err != nil {
				log.Printf("failed to delete CRD serviceID: %v", err)
			}
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	go ingInformer.Run(stopCh)
	go crdInformer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, ingInformer.HasSynced, crdInformer.HasSynced) {
		log.Fatal("failed to sync caches")
	}

	// Prober + API
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

	stopSig := make(chan os.Signal, 1)
	signal.Notify(stopSig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTP API listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stopSig
	log.Println("shutting down Evmon...")

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
}