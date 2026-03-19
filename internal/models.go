// internal/models.go
package internal

import "time"

// Service represents a monitored service (internal or external)
type Service struct {
	ID        string    `json:"id"`          // unique identifier
	Name      string    `json:"name"`        // human-readable name
	FirstSeen time.Time `json:"first_seen"`  // timestamp when first seen
}

// Status represents the current state of a service
type Status string

const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// Event represents a state change for a service
type Event struct {
	ServiceID string    `json:"service_id"` // references Service.ID
	Status    Status    `json:"status"`     // new state
	Timestamp time.Time `json:"timestamp"`  // when the change occurred
}

// Target represents an endpoint that can be probed
type Target struct {
	ServiceID string        `json:"service_id"`       // which service this target belongs to
	URL       string        `json:"url"`              // full URL for probing
	Internal  bool          `json:"internal"`         // true = internal probe, false = external
	Interval  time.Duration `json:"interval_seconds"` // optional probe interval, 0 = default
}

// Store defines the interface used by the prober and API
type Store interface {
	InsertEventIfChanged(serviceID string, status Status) error
	GetOrCreateService(name string) (*Service, error)
	ListServices() ([]Service, error)
	GetCurrentStatus(serviceID string) (Status, error)
	GetEventsInRange(serviceID string, from, to time.Time) ([]Event, error)
}