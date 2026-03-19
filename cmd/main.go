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
	"fmt"

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
	log.Println("evmon starting...")

	dbType := os.Getenv("EVMON_DB_TYPE")
	dbURL := os.Getenv("EVMON_DATABASE_URL")
	log.Printf("dbType=%s dbURL=%s", dbType, dbURL)

	var db *sql.DB
	var err error

	switch dbType {
	case "", "sqlite":
		if dbURL == "" {
			dbURL = "file:/data/evmon.db?mode=rwc&_foreign_keys=on&cache=shared"
		}
		log.Println("opening sqlite database")
		db, err = sql.Open("sqlite", dbURL)
	case "postgres":
		log.Println("opening postgres database")
		db, err = sql.Open("postgres", dbURL)
	case "mariadb":
		log.Println("opening mariadb database")
		db, err = sql.Open("mysql", dbURL)
	default:
		log.Fatalf("unsupported EVMON_DB_TYPE: %s", dbType)
	}
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer func() {
		log.Println("closing database")
		db.Close()
	}()

	store := internal.NewDBStore(db, dbType)
	log.Println("running migrations")
	if err := store.Migrate(); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	log.Println("creating controller")
	controller, err := internal.NewController()
	if err != nil {
		log.Fatalf("failed to create controller: %v", err)
	}

	log.Println("getting in-cluster config")
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get cluster config: %v", err)
	}

	log.Println("creating kubernetes clientset")
	clientset, _ := kubernetes.NewForConfig(config)
	log.Println("creating dynamic client")
	dynClient, _ := dynamic.NewForConfig(config)

	ctx := context.Background()

	// initial sync to populate controller
	log.Println("running initial SyncIngresses")
	controller.SyncIngresses(ctx)
	log.Println("running initial SyncCRDs")
	controller.SyncCRDs(ctx)
	log.Printf("initial targets loaded: %d", len(controller.ListTargets()))

	// cleanup
	log.Println("starting cleanup phase")
	existingServices, _ := store.ListServices()
	valid := map[string]struct{}{}
	for _, t := range controller.ListTargets() {
		valid[t.ServiceID] = struct{}{}
	}
	log.Printf("existing services in DB: %d", len(existingServices))
	for _, svc := range existingServices {
		if _, ok := valid[svc.ID]; !ok {
			log.Printf("deleting stale service: %s", svc.ID)
			store.DeleteService(svc.ID)
		}
	}

	// informers
	log.Println("setting up informers")
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Println("Ingress ADD event")
			ing := obj.(*networkingv1.Ingress)

			internalTargets := buildInternalTargets(clientset, ing)
			log.Printf("internal targets found: %d", len(internalTargets))
			for _, t := range internalTargets {
				log.Printf("adding internal target: %+v", t)
				controller.AddTarget(t)
				store.GetOrCreateService(t.ServiceID)
			}

			externalTargets := buildExternalTargets(ing)
			log.Printf("external targets found: %d", len(externalTargets))
			for _, t := range externalTargets {
				log.Printf("adding external target: %+v", t)
				controller.AddTarget(t)
				store.GetOrCreateService(t.ServiceID)
			}
		},
		DeleteFunc: func(obj interface{}) {
			log.Println("Ingress DELETE event")
			ing := obj.(*networkingv1.Ingress)

			for _, t := range buildInternalTargets(clientset, ing) {
				log.Printf("removing internal target: %+v", t)
				controller.RemoveTarget(t)
				store.DeleteService(t.ServiceID)
			}
			for _, t := range buildExternalTargets(ing) {
				log.Printf("removing external target: %+v", t)
				controller.RemoveTarget(t)
				store.DeleteService(t.ServiceID)
			}
		},
	})

	gvr := schema.GroupVersionResource{
		Group:    "evmon.centerionware.com",
		Version:  "v1",
		Resource: "evmonendpoints",
	}

	crdInf := dynamicinformer.NewFilteredDynamicInformer(
		dynClient, gvr, metav1.NamespaceAll, 0, cache.Indexers{}, nil,
	).Informer()

	crdInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Println("CRD ADD event")
			u := obj.(*unstructured.Unstructured)
			spec := u.Object["spec"].(map[string]interface{})

			serviceID := u.GetName()
			if v, ok := spec["serviceID"].(string); ok && v != "" {
				serviceID = v
			}

			url, _ := spec["url"].(string)

			log.Printf("adding CRD target: %s %s", serviceID, url)
			controller.AddTarget(internal.Target{
				ServiceID: serviceID,
				URL:       url,
				Internal:  false,
			})

			store.GetOrCreateService(serviceID)
		},
		DeleteFunc: func(obj interface{}) {
			log.Println("CRD DELETE event")
			u := obj.(*unstructured.Unstructured)
			spec := u.Object["spec"].(map[string]interface{})

			serviceID := u.GetName()
			if v, ok := spec["serviceID"].(string); ok && v != "" {
				serviceID = v
			}

			url, _ := spec["url"].(string)

			log.Printf("removing CRD target: %s %s", serviceID, url)
			controller.RemoveTarget(internal.Target{
				ServiceID: serviceID,
				URL:       url,
			})

			store.DeleteService(serviceID)
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	log.Println("starting informers")
	go ingInformer.Run(stopCh)
	go crdInf.Run(stopCh)

	cache.WaitForCacheSync(stopCh, ingInformer.HasSynced, crdInf.HasSynced)
	log.Println("cache sync completed")

	log.Println("starting prober")
	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	log.Println("setting up API")
	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// --- Re-added health endpoints ---
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		log.Println("health check hit")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("root endpoint hit")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{Addr: ":8080", Handler: mux}
	log.Println("starting HTTP server on :8080")
	log.Println("waiting for shutdown signal")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-sig
	log.Println("shutting down server")
	server.Shutdown(ctx)
	log.Println("evmon stopped")
}

// --- Helpers for targets ---
func buildInternalTargets(cs *kubernetes.Clientset, ing *networkingv1.Ingress) []internal.Target {
	var targets []internal.Target

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			svc := path.Backend.Service.Name
			port := path.Backend.Service.Port.Number

			if port == 0 && path.Backend.Service.Port.Name != "" {
				s, err := cs.CoreV1().Services(ing.Namespace).Get(context.TODO(), svc, metav1.GetOptions{})
				if err != nil {
					continue
				}
				for _, p := range s.Spec.Ports {
					if p.Name == path.Backend.Service.Port.Name {
						port = p.Port
					}
				}
			}

			if port == 0 {
				continue
			}

			targets = append(targets, internal.Target{
				ServiceID: fmt.Sprintf("%s/%s", ing.Namespace, svc),
				URL:       fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc, ing.Namespace, port),
				Internal:  true,
				Interval:  30 * time.Second,
			})
		}
	}

	return targets
}

func buildExternalTargets(ing *networkingv1.Ingress) []internal.Target {
	var targets []internal.Target

	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		targets = append(targets, internal.Target{
			ServiceID: "External/" + rule.Host,
			URL:       "https://" + rule.Host,
			Internal:  false,
			Interval:  300 * time.Second,
		})
	}

	return targets
}