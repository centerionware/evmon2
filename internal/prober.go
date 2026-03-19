// internal/prober.go
package internal

import (
    "crypto/tls"
	"net/http"
	"strings"
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
	running map[string]struct{}
}

// NewProber creates a new Prober
func NewProber(store Store, controller *Controller) *Prober {
	return &Prober{
		store:      store,
		controller: controller,
		httpClient: &http.Client{
            Timeout: 5 * time.Second,
            Transport: &http.Transport{
                TLSClientConfig: &tls.Config{
                    InsecureSkipVerify: true,
                },
            },
        },
		stopCh:     make(chan struct{}),
		running:    make(map[string]struct{}),
	}
}

// Start begins the probing loops
func (p *Prober) Start() {
	p.refreshTargets()

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

// refreshTargets starts probe loops for new targets only
func (p *Prober) refreshTargets() {
	targets := p.controller.ListTargets()

	for _, t := range targets {
		key := t.ServiceID + "|" + t.URL

		p.mu.Lock()
		if _, exists := p.running[key]; exists {
			p.mu.Unlock()
			continue
		}
		p.running[key] = struct{}{}
		p.mu.Unlock()

		p.wg.Add(1)
		go p.probeLoop(t, key)
	}
}

// probeLoop probes a single target repeatedly
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

// probeOnce performs a single HTTP request
func (p *Prober) probeOnce(url string) (bool, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// fallback to GET
		req, err = http.NewRequest("GET", url, nil)
		if err != nil {
			return false, err
		}
		resp, err = p.httpClient.Do(req)
		if err != nil {
			return false, err
		}
	}

	defer resp.Body.Close()

	return resp.StatusCode < 500, nil
}

// probeTarget performs a single probe and updates the store
func (p *Prober) probeTarget(target Target) {
	// Ensure service exists
	_, err := p.store.GetOrCreateService(target.ServiceID)
	if err != nil {
		println("error creating service:", err.Error())
		return
	}

	status := StatusDown
	raw := target.URL

	var urlsToTry []string

	// If scheme already present, just use it
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		urlsToTry = []string{raw}
	} else {
		// Try HTTP first, then HTTPS
		urlsToTry = []string{
			"http://" + raw,
			"https://" + raw,
		}
	}

	for _, url := range urlsToTry {
		println("probing:", target.ServiceID, url)

        ok, err := p.probeOnce(url)
        
        if err != nil {
        	println("probe error:", err.Error())
        } else if ok {
        	println("probe SUCCESS:", target.ServiceID)
        } else {
        	println("probe FAILED:", target.ServiceID)
        }
		if err == nil && ok {
			status = StatusUp
			break
		}
	}

	err = p.store.InsertEventIfChanged(target.ServiceID, status)
	if err != nil {
		println("error writing event:", err.Error())
	}
}
