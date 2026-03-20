// internal/api.go
package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type API struct {
	store *DBStore
    clientHook ClientHook
}

func NewAPI(store *DBStore) *API {
	return &API{
      store: store,
      clientHook: nil,
    }
}

func (s *API) SetClientHook(hook ClientHook) {
	s.clientHook = hook
}

func (api *API) RegisterRoutes(mux *http.ServeMux) {
	// Wrapper to enforce per-client PSK
	withClientAuth := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			clientID := r.Header.Get("X-Client-ID")
			psk := r.Header.Get("X-Client-PSK")
			if clientID == "" || psk == "" {
				http.Error(w, "missing client credentials", http.StatusUnauthorized)
				return
			}

			if api.clientHook == nil {
				http.Error(w, "server misconfigured", http.StatusInternalServerError)
				return
			}

			// Validate client
			c, err := api.clientHook.GetClient(clientID)
			if err != nil || c.ClientPSK != psk {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			handler(w, r)
		}
	}

	mux.HandleFunc("/status", withClientAuth(api.handleStatus))
	mux.HandleFunc("/history", withClientAuth(api.handleHistory))
}

func (api *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	type ServiceStatus struct {
		ServiceID string `json:"service_id"`
		Status    Status `json:"status"`
	}

	services, err := api.store.ListServices()
	if err != nil {
		http.Error(w, "error listing services: "+err.Error(), http.StatusInternalServerError)
		return
	}

	results := make([]ServiceStatus, 0, len(services))
	for _, svc := range services {
		status, err := api.store.GetCurrentStatus(svc.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "error fetching status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			status = ""
		}
		results = append(results, ServiceStatus{
			ServiceID: svc.ID,
			Status:    status,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (api *API) handleHistory(w http.ResponseWriter, r *http.Request) {
	// Optional query params
	serviceID := r.URL.Query().Get("service_id")
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	var from, to time.Time
	var err error

	// Default: last 12 hours
	if fromStr == "" {
		from = time.Now().Add(-12 * time.Hour)
	} else {
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			http.Error(w, "invalid from timestamp", http.StatusBadRequest)
			return
		}
	}

	if toStr == "" {
		to = time.Now()
	} else {
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			http.Error(w, "invalid to timestamp", http.StatusBadRequest)
			return
		}
	}

	var results map[string][]Event = make(map[string][]Event)

	if serviceID != "" {
		// Single service
		events, err := api.store.GetEventsInRange(serviceID, from, to)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "error fetching events: "+err.Error(), http.StatusInternalServerError)
			return
		}
		results[serviceID] = events
	} else {
		// All services
		services, err := api.store.ListServices()
		if err != nil {
			http.Error(w, "error fetching services: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, svc := range services {
			events, err := api.store.GetEventsInRange(svc.ID, from, to)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				continue // skip errors for individual services
			}
			results[svc.ID] = events
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}