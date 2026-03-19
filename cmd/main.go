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
	clientset, _ := kubernetes.NewForConfig(config)
	dynClient, _ := dynamic.NewForConfig(config)

	ctx := context.Background()

	// initial sync to populate controller
	controller.SyncIngresses(ctx)
	controller.SyncCRDs(ctx)

	// cleanup
	existingServices, _ := store.ListServices()
	valid := map[string]struct{}{}
	for _, t := range controller.ListTargets() {
		valid[t.ServiceID] = struct{}{}
	}
	for _, svc := range existingServices {
		if _, ok := valid[svc.ID]; !ok {
			store.DeleteService(svc.ID)
		}
	}

	// informers
	factory := informers.NewSharedInformerFactory(clientset, 0)
	ingInformer := factory.Networking().V1().Ingresses().Informer()

	ingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)

			for _, t := range buildInternalTargets(clientset, ing) {
				controller.AddTarget(t)
				store.GetOrCreateService(t.ServiceID)
			}
			for _, t := range buildExternalTargets(ing) {
				controller.AddTarget(t)
				store.GetOrCreateService(t.ServiceID)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*networkingv1.Ingress)

			for _, t := range buildInternalTargets(clientset, ing) {
				controller.RemoveTarget(t)
				store.DeleteService(t.ServiceID)
			}
			for _, t := range buildExternalTargets(ing) {
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
			u := obj.(*unstructured.Unstructured)
			spec := u.Object["spec"].(map[string]interface{})

			serviceID := u.GetName()
			if v, ok := spec["serviceID"].(string); ok && v != "" {
				serviceID = v
			}

			url, _ := spec["url"].(string)

			controller.AddTarget(internal.Target{
				ServiceID: serviceID,
				URL:       url,
				Internal:  false,
			})

			store.GetOrCreateService(serviceID)
		},
		DeleteFunc: func(obj interface{}) {
			u := obj.(*unstructured.Unstructured)
			spec := u.Object["spec"].(map[string]interface{})

			serviceID := u.GetName()
			if v, ok := spec["serviceID"].(string); ok && v != "" {
				serviceID = v
			}

			url, _ := spec["url"].(string)

			controller.RemoveTarget(internal.Target{
				ServiceID: serviceID,
				URL:       url,
			})

			store.DeleteService(serviceID)
		},
	})

	stopCh := make(chan struct{})
	defer close(stopCh)

	go ingInformer.Run(stopCh)
	go crdInf.Run(stopCh)

	cache.WaitForCacheSync(stopCh, ingInformer.HasSynced, crdInf.HasSynced)

	prober := internal.NewProber(store, controller)
	prober.Start()
	defer prober.Stop()

	api := internal.NewAPI(store)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	server := &http.Server{Addr: ":8080", Handler: mux}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go server.ListenAndServe()

	<-sig
	server.Shutdown(ctx)
}

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