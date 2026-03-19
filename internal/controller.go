package internal

import (
	"context"
	"fmt"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Controller maintains the list of targets
type Controller struct {
	clientset *kubernetes.Clientset
	dynClient dynamic.Interface
	targets   map[string]Target // key = serviceID+URL
	mu        sync.RWMutex
}

// NewController creates a new Controller
func NewController() (*Controller, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	dc, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &Controller{
		clientset: cs,
		dynClient: dc,
		targets:   make(map[string]Target),
	}, nil
}

// ListTargets returns the current list of targets
func (c *Controller) ListTargets() []Target {
	c.mu.RLock()
	defer c.mu.RUnlock()

	t := make([]Target, 0, len(c.targets))
	for _, target := range c.targets {
		t = append(t, target)
	}
	return t
}

// SyncIngresses fetches all Ingress resources and updates internal targets
func (c *Controller) SyncIngresses(ctx context.Context) error {
	ingresses, err := c.clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, ing := range ingresses.Items {
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				svcName := path.Backend.Service.Name
				port := path.Backend.Service.Port.Number
				serviceID := fmt.Sprintf("%s/%s", ing.Namespace, svcName)
				url := fmt.Sprintf("%s.%s.svc.cluster.local:%d", svcName, ing.Namespace, port)
				key := serviceID + url

				c.targets[key] = Target{
					ServiceID: serviceID,
					URL:       url,
					Internal:  true,
					Interval:  30 * time.Second, // internal endpoints always 30s
				}
			}
		}
	}

	return nil
}

// SyncCRDs fetches all EvmonEndpoint CRDs for external monitoring
func (c *Controller) SyncCRDs(ctx context.Context) error {
	evmonGVR := schema.GroupVersionResource{
		Group:    "evmon.centerionware.com",
		Version:  "v1",
		Resource: "evmonendpoints",
	}

	crds, err := c.dynClient.Resource(evmonGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, obj := range crds.Items {
		spec, ok := obj.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}

		url, ok := spec["url"].(string)
		if !ok || url == "" {
			continue
		}

		serviceID, ok := spec["serviceID"].(string)
		if !ok || serviceID == "" {
			serviceID = obj.GetName()
		}

		// Use CRD-defined intervalSeconds if provided; otherwise default to 300s (5min)
		interval := 300 // seconds
		if val, ok := spec["intervalSeconds"].(int64); ok && val > 0 {
			interval = int(val)
		} else if valf, ok := spec["intervalSeconds"].(float64); ok && valf > 0 {
			interval = int(valf)
		}

		key := serviceID + url
		c.targets[key] = Target{
			ServiceID: serviceID,
			URL:       url,
			Internal:  false,
			Interval:  time.Duration(interval) * time.Second, // use seconds directly
		}
	}

	return nil
}