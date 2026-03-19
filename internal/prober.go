// internal/prober.go
package internal

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Prober periodically probes targets and updates the store
type Prober struct {
	store      Store
	controller *Controller
	httpClient *http.Client
	wg         sync.WaitGroup
	stopCh     chan struct{}
}

// NewProber creates a new Prober
func NewProber(store Store, controller *Controller) *Prober {
	return &Prober{
		store:      store,
		controller: controller,
		httpClient: &http.Client{
			Timeout: 5 * time.Second, // quick fail on slow endpoints
		},
		stopCh: make(chan struct{}),
	}
}

// Start begins the probing loops
func (p *Prober) Start() {
	targets := p.controller.ListTargets()
	for _, target := range targets {
		p.wg.Add(1)
		go p.probeLoop(target)
	}

	// Optional: periodically refresh targets from controller
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

// refreshTargets adds new targets discovered by the controller
func (p *Prober) refreshTargets() {
	currentTargets := p.controller.ListTargets()
	for _, t := range currentTargets {
		// Could add logic to start new goroutine for new targets
		// For simplicity, assume targets do not change often for MVP
	}
}

// probeLoop probes a single target at the appropriate interval
func (p *Prober) probeLoop(target Target) {
	defer p.wg.Done()

	interval := 30 * time.Second
	if !target.Internal {
		interval = 5 * time.Minute
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
	status := StatusDown

	req, err := http.NewRequest("HEAD", target.URL, nil)
	if err != nil {
		// fallback to GET if HEAD fails
		req, _ = http.NewRequest("GET", target.URL, nil)
	}

	resp, err := p.httpClient.Do(req)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
		status = StatusUp
	}
	if resp != nil {
		resp.Body.Close()
	}

	// write event only if state changed
	err = p.store.InsertEventIfChanged(target.ServiceID, status)
	if err != nil {
		// log error for now (replace with proper logging later)
		println("error writing event:", err.Error())
	}
}