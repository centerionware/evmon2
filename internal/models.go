package internal

import "time"

// ---------------------------------------------------
// Existing Service Models
// ---------------------------------------------------

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

// ---------------------------------------------------
// Store Interface (unchanged)
// ---------------------------------------------------

type Store interface {
	InsertEventIfChanged(serviceID string, status Status) error
	GetOrCreateService(name string) (*Service, error)
	ListServices() ([]Service, error)
	GetCurrentStatus(serviceID string) (Status, error)
	GetEventsInRange(serviceID string, from, to time.Time) ([]Event, error)
	DeleteService(serviceID string) error
}

// ---------------------------------------------------
// New Client Models for client_hook.go
// ---------------------------------------------------

// ClientType defines whether a client is a UI or Notifier
type ClientType string

const (
	ClientTypeUI          ClientType = "ui"
	ClientTypeNotification ClientType = "notifications"
)

// Client represents a registered external client (UI or Notifier)
type Client struct {
	ClientID      string     `json:"client_id"`       // unique identifier
	Type          ClientType `json:"type"`            // "ui" or "notifications"
	CallbackURL   string     `json:"callback_url"`    // full URL for /update or /initialize
	CurrentPubKey string     `json:"current_pubkey"`  // base64 PQ public key
	ClientPSK     string     `json:"client_psk"`      // static PSK for /register and initial auth
	LastSeen      time.Time  `json:"last_seen"`       // last time this client pinged /register
}

// ClientPushInfo represents in-memory runtime info for push notifications
type ClientPushInfo struct {
	CurrentPubKey string // ephemeral PQ key for encryption
	NextPubKey    string // ephemeral next PQ key, rotates after each push
	CallbackURL   string // target endpoint for updates
}