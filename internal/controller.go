// internal/controller.go
package internal

import (
	"context"
	"fmt"
	"sync"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crdclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Controller maintains the list of targets
type Controller struct {
	clientset *kubernetes.Clientset
	targets   map[string]Target // key = ServiceID+URL
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

	return &Controller{
		clientset: cs,
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
				}
			}
		}
	}

	return nil
}

// SyncCRDs fetches all Evmon Endpoint CRDs for external monitoring
func (c *Controller) SyncCRDs(ctx context.Context) error {
	// NOTE: minimal placeholder, will need a proper CRD client
	// Using dynamic client or codegen in the future
	return nil
}