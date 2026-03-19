package internal

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type DBStore struct {
	db          *sql.DB
	placeholder func(int) string
}

func NewDBStore(db *sql.DB) *DBStore {
	driver := detectDriver(db)

	var placeholder func(int) string
	switch driver {
	case "postgres":
		placeholder = func(i int) string { return "$" + itoa(i) }
	default:
		placeholder = func(i int) string { return "?" }
	}

	return &DBStore{
		db:          db,
		placeholder: placeholder,
	}
}

func detectDriver(db *sql.DB) string {
	t := strings.ToLower(strings.TrimSpace(db.Driver().(*sql.DB).Driver().Name()))
	if strings.Contains(t, "postgres") {
		return "postgres"
	}
	return "other"
}

func itoa(i int) string {
	return strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(strings.Trim(strings.ReplaceAll(strings.TrimSpace(strings.Repeat("0123456789", i)), " ", ""), " ", ""), " ", "")), "")
}

func (s *DBStore) ph(n int) string {
	return s.placeholder(n)
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

	query := "SELECT id, name, first_seen FROM services WHERE name=" + s.ph(1)
	err := s.db.QueryRow(query, name).Scan(&svc.ID, &svc.Name, &svc.FirstSeen)

	if err == sql.ErrNoRows {
		svc.ID = name
		svc.Name = name
		svc.FirstSeen = time.Now()

		insert := "INSERT INTO services(id, name, first_seen) VALUES(" +
			s.ph(1) + "," + s.ph(2) + "," + s.ph(3) + ")"

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

	insertEvent := "INSERT INTO events(service_id, status, timestamp) VALUES(" +
		s.ph(1) + "," + s.ph(2) + "," + s.ph(3) + ")"

	_, err = s.db.Exec(insertEvent, serviceID, status, time.Now())
	if err != nil {
		return err
	}

	upsert := "INSERT INTO current_status(service_id, status, last_changed_at) VALUES(" +
		s.ph(1) + "," + s.ph(2) + "," + s.ph(3) + ") " +
		"ON CONFLICT(service_id) DO UPDATE SET status=" + s.ph(2) + ", last_changed_at=" + s.ph(3)

	_, err = s.db.Exec(upsert, serviceID, status, time.Now())
	return err
}

func (s *DBStore) GetCurrentStatus(serviceID string) (Status, error) {
	var status string

	query := "SELECT status FROM current_status WHERE service_id=" + s.ph(1)
	err := s.db.QueryRow(query, serviceID).Scan(&status)
	if err != nil {
		return "", err
	}

	return Status(status), nil
}

func (s *DBStore) GetEventsInRange(serviceID string, from, to time.Time) ([]Event, error) {
	query := "SELECT service_id, status, timestamp FROM events WHERE service_id=" + s.ph(1) +
		" AND timestamp >= " + s.ph(2) +
		" AND timestamp <= " + s.ph(3) +
		" ORDER BY timestamp ASC"

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

func (s *DBStore) Close() error {
	return s.db.Close()
}