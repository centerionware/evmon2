// internal/store.go
package internal

import (
	"database/sql"
	"errors"
	"time"
)

// DBStore is a generic SQL-backed store
type DBStore struct {
	db *sql.DB
}

// NewDBStore creates a new DBStore from an *sql.DB
func NewDBStore(db *sql.DB) *DBStore {
	return &DBStore{db: db}
}

// GetOrCreateService returns the service if it exists, or creates it
func (s *DBStore) GetOrCreateService(name string) (*Service, error) {
	var svc Service
	err := s.db.QueryRow("SELECT id, name, first_seen FROM services WHERE name=$1", name).
		Scan(&svc.ID, &svc.Name, &svc.FirstSeen)
	if err == sql.ErrNoRows {
		// insert new service
		svc.ID = name
		svc.Name = name
		svc.FirstSeen = time.Now()
		_, err := s.db.Exec("INSERT INTO services(id, name, first_seen) VALUES($1,$2,$3)",
			svc.ID, svc.Name, svc.FirstSeen)
		if err != nil {
			return nil, err
		}
		return &svc, nil
	} else if err != nil {
		return nil, err
	}
	return &svc, nil
}

// InsertEventIfChanged inserts a new event only if the status changed
func (s *DBStore) InsertEventIfChanged(serviceID string, status Status) error {
	current, err := s.GetCurrentStatus(serviceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current == status {
		return nil
	}

	_, err = s.db.Exec("INSERT INTO events(service_id, status, timestamp) VALUES($1,$2,$3)",
		serviceID, status, time.Now())
	if err != nil {
		return err
	}

	_, err = s.db.Exec("INSERT INTO current_status(service_id, status, last_changed_at) "+
		"VALUES($1,$2,$3) ON CONFLICT(service_id) DO UPDATE SET status=$2, last_changed_at=$3",
		serviceID, status, time.Now())
	return err
}

// GetCurrentStatus fetches the current status for a service
func (s *DBStore) GetCurrentStatus(serviceID string) (Status, error) {
	var status string
	err := s.db.QueryRow("SELECT status FROM current_status WHERE service_id=$1", serviceID).Scan(&status)
	if err != nil {
		return "", err
	}
	return Status(status), nil
}

// GetEventsInRange fetches events between from and to timestamps
func (s *DBStore) GetEventsInRange(serviceID string, from, to time.Time) ([]Event, error) {
	rows, err := s.db.Query("SELECT service_id, status, timestamp FROM events "+
		"WHERE service_id=$1 AND timestamp >= $2 AND timestamp <= $3 ORDER BY timestamp ASC",
		serviceID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ServiceID, &e.Status, &e.Timestamp); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// Close closes the underlying DB
func (s *DBStore) Close() error {
	return s.db.Close()
}