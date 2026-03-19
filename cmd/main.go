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

	// --- Initial cleanup ---
	existingServices, err := store.ListServices()
	if err != nil {
		log.Printf("failed to list services for cleanup: %v", err)
	} else {
		cleanupMap := map[string]struct{}{}
		for _, svc := range existingServices {
			cleanupMap[svc.ID] = struct{}{}
		}

		// Scan cluster ingresses
		ings, err := clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, ing := range ings.Items {
				for _, rule := range ing.Spec.Rules {
					if rule.HTTP == nil {
						continue
					}
					for _, path := range rule.HTTP.Paths {
						svcID := fmt.Sprintf("%s/%s", ing.Namespace, path.Backend.Service.Name)
						delete(cleanupMap, svcID)
						if rule.Host != "" {
							delete(cleanupMap, "External/"+rule.Host)
						}
					}
				}
			}
		}

		// Scan CRDs
		evmonGVR := schema.GroupVersionResource{
			Group:    "evmon.centerionware.com",
			Version:  "v1",
			Resource: "evmonendpoints",
		}
		crds, err := dynClient.Resource(evmonGVR).Namespace("").List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, obj := range crds.Items {
				spec, ok := obj.Object["spec"].(map[string]interface{})
				if !ok {
					continue
				}
				svcID, ok := spec["serviceID"].(string)
				if ok && svcID != "" {
					delete(cleanupMap, svcID)
				}
			}
		}

		// Delete remaining stale services
		for id := range cleanupMap {
			if err := store.DeleteService(id); err != nil {
				log.Printf("failed to delete stale service %s: %v", id, err)
			}
		}
	}

	// --- Ingress informer ---
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			internalTargets := buildInternalTargets(controller, clientset, ing)
			for _, t := range internalTargets {
				if _, err := store.GetOrCreateService(t.ServiceID); err != nil {
					log.Printf("failed to create internal service: %v", err)
				}
			}

			externalTargets := buildExternalTargets(ing)
			for _, t := range externalTargets {
				if _, err := store.GetOrCreateService(t.ServiceID); err != nil {
					log.Printf("failed to create external service: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)
			for _, t := range buildInternalTargets(controller, clientset, ing) {
				store.DeleteService(t.ServiceID)
			}
			for _, t := range buildExternalTargets(ing) {
				store.DeleteService(t.ServiceID)
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
	crdInformerInf := crdInformer.Informer()

	crdInformerInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)
			spec, ok := u.Object["spec"].(map[string]interface{})
			if !ok {
				return
			}
			serviceID, ok := spec["serviceID"].(string)
			if !ok || serviceID == "" {
				serviceID = u.GetName()
			}
			if _, err := store.GetOrCreateService(serviceID); err != nil {
				log.Printf("failed to create CRD service: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)
			spec, ok := u.Object["spec"].(map[string]interface{})
			if !ok {
				return
			}
			serviceID, ok := spec["serviceID"].(string)
			if !ok || serviceID == "" {
				serviceID = u.GetName()
			}
			if err := store.DeleteService(serviceID); err != nil {
				log.Printf("failed to delete CRD service: %v", err)
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

	go server.ListenAndServe()

	<-stopSig
	log.Println("shutting down...")
	server.Shutdown(ctx)
}

// --- Helpers for targets ---
func buildInternalTargets(c *internal.Controller, cs *kubernetes.Clientset, ing *networkingv1.Ingress) []internal.Target {
	var targets []internal.Target
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			svcName := path.Backend.Service.Name
			port := path.Backend.Service.Port.Number
			if port == 0 && path.Backend.Service.Port.Name != "" {
				svc, err := cs.CoreV1().Services(ing.Namespace).Get(context.TODO(), svcName, metav1.GetOptions{})
				if err != nil {
					continue
				}
				for _, p := range svc.Spec.Ports {
					if p.Name == path.Backend.Service.Port.Name {
						port = p.Port
						break
					}
				}
			}
			if port == 0 {
				continue
			}
			serviceID := fmt.Sprintf("%s/%s", ing.Namespace, svcName)
			url := fmt.Sprintf("%s.%s.svc.cluster.local:%d", svcName, ing.Namespace, port)
			targets = append(targets, internal.Target{
				ServiceID: serviceID,
				URL:       url,
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
		svcID := "External/" + rule.Host
		url := "https://" + rule.Host
		targets = append(targets, internal.Target{
			ServiceID: svcID,
			URL:       url,
			Internal:  false,
			Interval:  300 * time.Second,
		})
	}
	return targets
}