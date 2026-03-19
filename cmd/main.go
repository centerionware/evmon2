package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "modernc.org/sqlite"
	_ "github.com/lib/pq"
	_ "github.com/go-sql-driver/mysql"

	"centerionware.com/evmon/internal"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/rest"
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
		db, err = sql.Open("postgres", dbURL)
	case "mariadb":
		db, err = sql.Open("mysql", dbURL)
	default:
		log.Fatalf("unsupported EVMON_DB_TYPE: %s", dbType)
	}
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	store := internal.NewDBStore(db, dbType)
	if err := store.Migrate(); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	// Kubernetes clients
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

	// --- Ingress informer ---
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			serviceIDs := extractIngressServiceIDs(ing)
			for _, id := range serviceIDs {
				if _, err := store.GetOrCreateService(id); err != nil {
					log.Printf("failed to create service from ingress: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			serviceIDs := extractIngressServiceIDs(ing)
			for _, id := range serviceIDs {
				if err := store.DeleteService(id); err != nil {
					log.Printf("failed to delete service from ingress: %v", err)
				}
			}
		},
	})

	// --- CRD informer ---
	gvr := schema.GroupVersionResource{
		Group:    "evmon.centerionware.com",
		Version:  "v1",
		Resource: "evmonendpoints",
	}

	crdInformer := dynamicinformer.NewFilteredDynamicInformer(
		dynClient,
		gvr,
		metav1.NamespaceAll,
		0,
		cache.Indexers{},
		nil,
	)
	crdInformerInf := crdInformer.Informer() // Important: get underlying informer

	crdInformerInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)
			serviceID, _, _ := unstructured.NestedString(u.Object, "spec", "serviceID")
			if serviceID == "" {
				return
			}
			if _, err := store.GetOrCreateService(serviceID); err != nil {
				log.Printf("failed to create service from CRD: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)
			serviceID, _, _ := unstructured.NestedString(u.Object, "spec", "serviceID")
			if serviceID == "" {
				return
			}
			if err := store.DeleteService(serviceID); err != nil {
				log.Printf("failed to delete service from CRD: %v", err)
			}
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	go ingInformer.Run(stopCh)
	go crdInformerInf.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, ingInformer.HasSynced, crdInformerInf.HasSynced) {
		log.Fatal("failed to sync caches")
	}

	// --- Prober ---
	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	// --- API ---
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

	go server.ListenAndServe()

	<-stopSig
	log.Println("shutting down...")
	server.Shutdown(ctx)
}

// --- helper to extract service IDs from an ingress ---
func extractIngressServiceIDs(ing *networkingv1.Ingress) []string {
	var ids []string
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			ids = append(ids, rule.Host)
		}
	}
	return ids
}