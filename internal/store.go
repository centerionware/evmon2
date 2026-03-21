// internal/store.go
package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
    "encoding/json"
)


// DBStore is a SQL-backed implementation of Store
type DBStore struct {
	db     *sql.DB
	dbType string
    clientHook ClientHook
}

func NewDBStore(db *sql.DB, dbType string) *DBStore {
	return &DBStore{
		db:     db,
		dbType: dbType,
        clientHook: nil,
	}
}

func (s *DBStore) SetClientHook(hook ClientHook) {
	s.clientHook = hook
}

func (s *DBStore) placeholder(n int) string {
	if s.dbType == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (s *DBStore) Migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS services (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			first_seen TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS events (
			service_id TEXT NOT NULL,
			status TEXT NOT NULL,
			timestamp TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS current_status (
			service_id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			last_changed_at TIMESTAMP NOT NULL
		);`,
	}

	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
}

func (s *DBStore) ListServices() ([]Service, error) {
	rows, err := s.db.Query("SELECT id, name, first_seen FROM services ORDER BY name ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var services []Service
	for rows.Next() {
		var svc Service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.FirstSeen); err != nil {
			return nil, err
		}
		services = append(services, svc)
	}
	return services, nil
}

func (s *DBStore) GetOrCreateService(name string) (*Service, error) {
	var svc Service

	query := fmt.Sprintf("SELECT id, name, first_seen FROM services WHERE name=%s", s.placeholder(1))
	err := s.db.QueryRow(query, name).Scan(&svc.ID, &svc.Name, &svc.FirstSeen)

	if err == sql.ErrNoRows {
		svc.ID = name
		svc.Name = name
		svc.FirstSeen = time.Now()

		insert := fmt.Sprintf(
			"INSERT INTO services(id, name, first_seen) VALUES(%s,%s,%s)",
			s.placeholder(1), s.placeholder(2), s.placeholder(3),
		)

		_, err := s.db.Exec(insert, svc.ID, svc.Name, svc.FirstSeen)
		if err != nil {
			return nil, err
		}
		return &svc, nil
	} else if err != nil {
		return nil, err
	}

	return &svc, nil
}

func (s *DBStore) InsertEventIfChanged(serviceID string, status Status) error {
	current, err := s.GetCurrentStatus(serviceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if current == status {
		return nil
	}

	now := time.Now()

	insertEvent := fmt.Sprintf(
		"INSERT INTO events(service_id, status, timestamp) VALUES(%s,%s,%s)",
		s.placeholder(1), s.placeholder(2), s.placeholder(3),
	)

	_, err = s.db.Exec(insertEvent, serviceID, status, now)
	if err != nil {
		return err
	}

	var upsert string
	if s.dbType == "postgres" {
		upsert = fmt.Sprintf(
			"INSERT INTO current_status(service_id, status, last_changed_at) VALUES(%s,%s,%s) "+
				"ON CONFLICT(service_id) DO UPDATE SET status=%s, last_changed_at=%s",
			s.placeholder(1), s.placeholder(2), s.placeholder(3),
			s.placeholder(2), s.placeholder(3),
		)
	} else {
		upsert = fmt.Sprintf(
			"INSERT OR REPLACE INTO current_status(service_id, status, last_changed_at) VALUES(%s,%s,%s)",
			s.placeholder(1), s.placeholder(2), s.placeholder(3),
		)
	}

    // ---------------------------------------------------
	// Trigger async push to all registered clients
	// ---------------------------------------------------
	if s.clientHook != nil {
		go func() {
			payload := map[string]interface{}{
				"service_id": serviceID,
				"status":     status,
				"timestamp":  now,
			}
			data, _ := json.Marshal(payload)
			_ = s.clientHook.SendPushToAll(data) // ignore errors for async broadcast
		}()
	}

	_, err = s.db.Exec(upsert, serviceID, status, now)
	return err
}

func (s *DBStore) GetCurrentStatus(serviceID string) (Status, error) {
	var status string

	query := fmt.Sprintf(
		"SELECT status FROM current_status WHERE service_id=%s",
		s.placeholder(1),
	)

	err := s.db.QueryRow(query, serviceID).Scan(&status)
	if err != nil {
		return "", err
	}

	return Status(status), nil
}

func (s *DBStore) GetEventsInRange(serviceID string, from, to time.Time) ([]Event, error) {
	query := fmt.Sprintf(
		`SELECT service_id, status, timestamp 
		 FROM events 
		 WHERE service_id=%s AND timestamp >= %s AND timestamp <= %s 
		 ORDER BY timestamp ASC`,
		s.placeholder(1), s.placeholder(2), s.placeholder(3),
	)

	rows, err := s.db.Query(query, serviceID, from, to)
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

func (s *DBStore) DeleteService(serviceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	queries := []string{
		"DELETE FROM events WHERE service_id=" + s.placeholder(1),
		"DELETE FROM current_status WHERE service_id=" + s.placeholder(1),
		"DELETE FROM services WHERE id=" + s.placeholder(1),
	}

	for _, q := range queries {
		if _, err := tx.Exec(q, serviceID); err != nil {
			// swallow errors (row may not exist — that's fine)
			continue
		}
	}

	return tx.Commit()
}

func (s *DBStore) Close() error {
	return s.db.Close()
}
