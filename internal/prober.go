package internal

import (
	_ "context" // imported for future use
	"net/http"
	"sync"
	"time"
)

// Status represents the status of a target
type Status string

const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// Target represents a service to probe
type Target struct {
	ServiceID int
	URL       string
	Internal  bool
	Interval  time.Duration // probe interval for this target
}

// Store is an interface for writing probe events
type Store interface {
	InsertEventIfChanged(serviceID int, status Status) error
}

// Controller manages the list of targets
type Controller struct {
	// ListTargets returns the current targets to probe
	ListTargets func() []Target

	// internal map to track which targets are already being probed
	targetMap map[string]struct{}
}

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
	if controller.targetMap == nil {
		controller.targetMap = make(map[string]struct{})
	}
	return &Prober{
		store:      store,
		controller: controller,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
}

// Start begins the probing loops
func (p *Prober) Start() {
	targets := p.controller.ListTargets()
	for _, target := range targets {
		p.controller.targetMap[target.URL] = struct{}{}
		p.wg.Add(1)
		go p.probeLoop(target)
	}

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
	currentTargets := p.controller.ListTargets()

	for _, t := range currentTargets {
		if _, ok := p.controller.targetMap[t.URL]; !ok {
			// New target discovered
			p.controller.targetMap[t.URL] = struct{}{}
			p.wg.Add(1)
			go p.probeLoop(t)
		}
	}
}

// probeLoop probes a single target at the interval defined by the controller
func (p *Prober) probeLoop(target Target) {
	defer p.wg.Done()

	interval := target.Interval
	if interval <= 0 {
		// fallback if controller didn't set an interval
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