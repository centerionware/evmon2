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
}

func NewAPI(store *DBStore) *API {
	return &API{store: store}
}

func (api *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/status", api.handleStatus)
	mux.HandleFunc("/history", api.handleHistory)
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
	serviceID := r.URL.Query().Get("service_id")
	if serviceID == "" {
		http.Error(w, "missing service_id", http.StatusBadRequest)
		return
	}

	var from, to time.Time
	var err error

	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	if fromStr == "" {
		from = time.Now().Add(-24 * time.Hour)
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

	events, err := api.store.GetEventsInRange(serviceID, from, to)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			events = []Event{}
		} else {
			http.Error(w, "error fetching events: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}