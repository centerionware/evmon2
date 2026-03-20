package internal

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cloudflare/circl/kem/kyber/kyber512"
	"github.com/google/uuid"
)

// ---------------------------------------------------
// ClientHook Implementation
// ---------------------------------------------------

type ClientHookImpl struct {
	db     *sql.DB
	dbType string
	mu     sync.Mutex
	// runtime in-memory map of client push info
	clients map[string]*ClientPushInfo
}

// Constructor
func NewClientHook(db *sql.DB, dbType string) *ClientHookImpl {
	return &ClientHookImpl{
		db:      db,
		dbType:  dbType,
		clients: make(map[string]*ClientPushInfo),
	}
}

// ---------------------------------------------------
// Migration
// ---------------------------------------------------

func (c *ClientHookImpl) MigrateClients() error {
	stmt := `
	CREATE TABLE IF NOT EXISTS clients (
		client_id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		callback_url TEXT NOT NULL,
		current_pubkey TEXT NOT NULL,
		client_psk TEXT NOT NULL,
		last_seen TIMESTAMP NOT NULL
	);`
	_, err := c.db.Exec(stmt)
	return err
}

// ---------------------------------------------------
// Utilities
// ---------------------------------------------------

func generatePSK(n int) (string, error) {
	key := make([]byte, n)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func generateClientID() string {
	return uuid.New().String()
}

// ---------------------------------------------------
// Create Client (/create_client) - admin only
// ---------------------------------------------------

func (c *ClientHookImpl) CreateClient() (*Client, error) {
	clientID := generateClientID()
	psk, err := generatePSK(32)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	_, err = c.db.Exec(
		`INSERT INTO clients(client_id, type, callback_url, current_pubkey, client_psk, last_seen)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		clientID, "", "", "", psk, now,
	)
	if err != nil {
		return nil, err
	}

	return &Client{
		ClientID:  clientID,
		ClientPSK: psk,
	}, nil
}

// ---------------------------------------------------
// Register Client (/register) - per-client PSK
// ---------------------------------------------------

func (c *ClientHookImpl) RegisterClient(clientID, psk, clientType, callbackURL, publicKey string) error {
	var storedPSK string
	err := c.db.QueryRow("SELECT client_psk FROM clients WHERE client_id=?", clientID).Scan(&storedPSK)
	if err != nil {
		return fmt.Errorf("client not found")
	}
	if storedPSK != psk {
		return fmt.Errorf("unauthorized: invalid PSK")
	}

	now := time.Now()
	_, err = c.db.Exec(
		`UPDATE clients SET type=?, callback_url=?, current_pubkey=?, last_seen=? WHERE client_id=?`,
		clientType, callbackURL, publicKey, now, clientID,
	)
	if err != nil {
		return err
	}

	// Update in-memory runtime map
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clients[clientID] = &ClientPushInfo{
		CurrentPubKey: publicKey,
		NextPubKey:    "",
		CallbackURL:   callbackURL,
	}
	return nil
}

// ---------------------------------------------------
// Get / List Clients
// ---------------------------------------------------

func (c *ClientHookImpl) GetClient(clientID string) (*Client, error) {
	row := c.db.QueryRow(`SELECT client_id, type, callback_url, current_pubkey, client_psk, last_seen FROM clients WHERE client_id=?`, clientID)
	var cli Client
	err := row.Scan(&cli.ClientID, &cli.Type, &cli.CallbackURL, &cli.CurrentPubKey, &cli.ClientPSK, &cli.LastSeen)
	if err != nil {
		return nil, err
	}
	return &cli, nil
}

func (c *ClientHookImpl) ListClients() ([]Client, error) {
	rows, err := c.db.Query(`SELECT client_id, type, callback_url, current_pubkey, client_psk, last_seen FROM clients`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clients []Client
	for rows.Next() {
		var cli Client
		if err := rows.Scan(&cli.ClientID, &cli.Type, &cli.CallbackURL, &cli.CurrentPubKey, &cli.ClientPSK, &cli.LastSeen); err != nil {
			return nil, err
		}
		clients = append(clients, cli)
	}
	return clients, nil
}

// ---------------------------------------------------
// PQ Encryption Helper
// ---------------------------------------------------

func encryptWithPQ(clientPubKeyB64 string, message []byte) (string, error) {
	pubBytes, err := base64.StdEncoding.DecodeString(clientPubKeyB64)
	if err != nil {
		return "", err
	}

	scheme := kyber512.Scheme()
	pubKey, err := scheme.UnmarshalBinaryPublicKey(pubBytes)
	if err != nil {
		return "", err
	}

	ct, sharedSecret, err := scheme.Encapsulate(pubKey)
	if err != nil {
		return "", err
	}

	// Simple XOR for demonstration (replace with AEAD in production)
	ciphertext := make([]byte, len(message))
	for i := range message {
		ciphertext[i] = message[i] ^ sharedSecret[i%len(sharedSecret)]
	}

	combined := append(ct, ciphertext...)
	return base64.StdEncoding.EncodeToString(combined), nil
}

// ---------------------------------------------------
// Push Logic
// ---------------------------------------------------

func (c *ClientHookImpl) SendPush(clientID string, payload []byte) error {
	c.mu.Lock()
	info, ok := c.clients[clientID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("client not found")
	}

	encryptedB64, err := encryptWithPQ(info.CurrentPubKey, payload)
	if err != nil {
		return err
	}

	pushBody := map[string]string{
		"event_id": uuid.New().String(),
		"type":     "change",
		"payload":  encryptedB64,
	}

	data, _ := json.Marshal(pushBody)
	req, err := http.NewRequest("POST", info.CallbackURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push failed %d", resp.StatusCode)
	}

	// Handle NextPublicKey rotation if client sends it
	body, _ := io.ReadAll(resp.Body)
	var pr struct {
		NextPublicKey string `json:"next_public_key"`
	}
	if err := json.Unmarshal(body, &pr); err == nil && pr.NextPublicKey != "" {
		c.mu.Lock()
		if info, ok := c.clients[clientID]; ok {
			info.CurrentPubKey = pr.NextPublicKey
			info.NextPubKey = ""
		}
		c.mu.Unlock()
	}

	return nil
}

func (c *ClientHookImpl) SendPushToAll(payload []byte) error {
	c.mu.Lock()
	clients := make([]*ClientPushInfo, 0, len(c.clients))
	ids := make([]string, 0, len(c.clients))
	for id, info := range c.clients {
		clients = append(clients, info)
		ids = append(ids, id)
	}
	c.mu.Unlock()

	var wg sync.WaitGroup
	for i, info := range clients {
		wg.Add(1)
		go func(clientID string, info *ClientPushInfo) {
			defer wg.Done()
			_ = c.SendPush(clientID, payload) // ignore errors for async broadcast
		}(ids[i], info)
	}
	wg.Wait()
	return nil
}

// ---------------------------------------------------
// RegisterRoutes
// ---------------------------------------------------

func (c *ClientHookImpl) RegisterRoutes(mux *http.ServeMux, adminPSK string) {
	mux.HandleFunc("/create_client", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-PSK") != adminPSK {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		client, err := c.CreateClient()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create client: %v", err), http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(client)
	})

	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ClientID    string `json:"client_id"`
			ClientType  string `json:"type"`
			CallbackURL string `json:"callback_url"`
			PubKey      string `json:"public_key"`
			PSK         string `json:"psk"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		if err := c.RegisterClient(req.ClientID, req.PSK, req.ClientType, req.CallbackURL, req.PubKey); err != nil {
			http.Error(w, fmt.Sprintf("registration failed: %v", err), http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	// /update endpoint stub (clients implement their own /update to receive pushes)
	mux.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
}