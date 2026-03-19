package internal

import (
	"database/sql"
	"errors"
	"time"
)

type DBStore struct {
	db *sql.DB
}

func NewDBStore(db *sql.DB) *DBStore {
	return &DBStore{db: db}
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

func (s *DBStore) GetOrCreateService(name string) (*Service, error) {
	var svc Service
	err := s.db.QueryRow("SELECT id, name, first_seen FROM services WHERE name=$1", name).
		Scan(&svc.ID, &svc.Name, &svc.FirstSeen)
	if err == sql.ErrNoRows {
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

func (s *DBStore) GetCurrentStatus(serviceID string) (Status, error) {
	var status string
	err := s.db.QueryRow("SELECT status FROM current_status WHERE service_id=$1", serviceID).Scan(&status)
	if err != nil {
		return "", err
	}
	return Status(status), nil
}

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

func (s *DBStore) Close() error {
	return s.db.Close()
}