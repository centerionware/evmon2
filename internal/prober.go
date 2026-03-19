```go
// internal/prober.go
package internal

import (
	"net/http"
	"sync"
	"time"
)

// Prober periodically probes targets and updates the store
type Prober struct {
	store      Store
	controller *Controller
	httpClient *http.Client

	wg     sync.WaitGroup
	stopCh chan struct{}

	mu      sync.Mutex
	running map[string]struct{} // tracks active probe loops
}

// NewProber creates a new Prober
func NewProber(store Store, controller *Controller) *Prober {
	return &Prober{
		store:      store,
		controller: controller,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		stopCh:     make(chan struct{}),
		running:    make(map[string]struct{}),
	}
}

// Start begins the probing loops
func (p *Prober) Start() {
	p.refreshTargets()

	// periodically refresh targets
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.refreshTargets()
			}
		}
	}()
}

// Stop stops all probe loops
func (p *Prober) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

// refreshTargets starts probe loops for any new targets discovered
func (p *Prober) refreshTargets() {
	targets := p.controller.ListTargets()

	for _, t := range targets {
		key := t.ServiceID + "|" + t.URL

		p.mu.Lock()
		if _, exists := p.running[key]; exists {
			p.mu.Unlock()
			continue // already probing this target
		}
		p.running[key] = struct{}{}
		p.mu.Unlock()

		p.wg.Add(1)
		go p.probeLoop(t, key)
	}
}

// probeLoop probes a single target at the interval defined by the controller
func (p *Prober) probeLoop(target Target, key string) {
	defer p.wg.Done()

	interval := target.Interval
	if interval <= 0 {
		if target.Internal {
			interval = 30 * time.Second
		} else {
			interval = 5 * time.Minute
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.probeTarget(target)
		}
	}
}

// probeTarget performs a single probe and updates the store
func (p *Prober) probeTarget(target Target) {
	// ✅ Ensure service exists
	_, err := p.store.GetOrCreateService(target.ServiceID)
	if err != nil {
		println("error creating service:", err.Error())
		return
	}

	status := StatusDown

	req, err := http.NewRequest("HEAD", target.URL, nil)
	if err != nil {
		req, _ = http.NewRequest("GET", target.URL, nil)
	}

	resp, err := p.httpClient.Do(req)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
		status = StatusUp
	}
	if resp != nil {
		resp.Body.Close()
	}

	err = p.store.InsertEventIfChanged(target.ServiceID, status)
	if err != nil {
		println("error writing event:", err.Error())
	}
}
```
