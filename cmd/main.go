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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"

	networkingv1 "k8s.io/api/networking/v1"
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

	// --- Kubernetes clients ---
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get cluster config: %v", err)
	}

	clientset, _ := kubernetes.NewForConfig(config)
	dynClient, _ := dynamic.NewForConfig(config)

	// --- Ingress informer ---
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)

			targets := controller.ExtractIngress(ing)

			for _, t := range targets {
				if _, err := store.GetOrCreateService(t.ServiceID); err != nil {
					log.Printf("failed to create service: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)

			ids := controller.ExtractIngressServiceIDs(ing)

			for _, id := range ids {
				if err := store.DeleteService(id); err != nil {
					log.Printf("failed to delete service: %v", err)
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

	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)

			serviceID, _, _ := unstructured.NestedString(u.Object, "spec", "serviceID")

			if _, err := store.GetOrCreateService(serviceID); err != nil {
				log.Printf("failed to create service: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)

			serviceID, _, _ := unstructured.NestedString(u.Object, "spec", "serviceID")

			if err := store.DeleteService(serviceID); err != nil {
				log.Printf("failed to delete service: %v", err)
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

	// --- Prober ---
	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	// --- API ---
	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	stopSig := make(chan os.Signal, 1)
	signal.Notify(stopSig, syscall.SIGINT, syscall.SIGTERM)

	go server.ListenAndServe()

	<-stopSig
	log.Println("shutting down...")
	server.Shutdown(context.Background())
}