package internal

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cloudflare/circl/kem/kyber/kyber512"
	"github.com/google/uuid"
)

// ---------------------------------------------------
// Database Migration
// ---------------------------------------------------

func (s *DBStore) MigrateClients() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS clients (
			client_id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			callback_url TEXT NOT NULL,
			current_pubkey TEXT NOT NULL,
			client_psk TEXT NOT NULL,
			last_seen TIMESTAMP NOT NULL
		);`,
	}

	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
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
// /create_client Handler (admin only)
// ---------------------------------------------------

func (s *DBStore) CreateClientHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := generateClientID()
		psk, err := generatePSK(32)
		if err != nil {
			http.Error(w, "failed to generate PSK", http.StatusInternalServerError)
			return
		}

		now := time.Now()
		_, err = s.db.Exec(
			`INSERT INTO clients(client_id, type, callback_url, current_pubkey, client_psk, last_seen) 
			 VALUES(?, ?, ?, ?, ?, ?)`,
			clientID, "", "", "", psk, now,
		)
		if err != nil {
			http.Error(w, "failed to store client", http.StatusInternalServerError)
			return
		}

		resp := map[string]string{
			"client_id":  clientID,
			"client_psk": psk,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// ---------------------------------------------------
// /register Handler
// ---------------------------------------------------

type RegisterRequest struct {
	Type      string `json:"type"`         // "ui" or "notifications"
	Callback  string `json:"callback_url"` // full URL to /update or /initialize
	PublicKey string `json:"public_key"`   // base64 ephemeral PQ public key
}

type RegisterResponse struct {
	ClientID string `json:"client_id"`
	Ack      string `json:"ack"`
}

func (s *DBStore) RegisterHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := r.Header.Get("X-Client-ID")
		psk := r.Header.Get("X-Evmon-Key")
		if clientID == "" || psk == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var storedPSK string
		err := s.db.QueryRow(
			"SELECT client_psk FROM clients WHERE client_id=?",
			clientID,
		).Scan(&storedPSK)
		if err != nil || storedPSK != psk {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		now := time.Now()
		_, err = s.db.Exec(
			`UPDATE clients SET type=?, callback_url=?, current_pubkey=?, last_seen=? WHERE client_id=?`,
			req.Type, req.Callback, req.PublicKey, now, clientID,
		)
		if err != nil {
			http.Error(w, "failed to update client", http.StatusInternalServerError)
			return
		}

		// Initialize in-memory push info
		clientPushMap[clientID] = &ClientPushInfo{
			CurrentPubKey: req.PublicKey,
			NextPubKey:    "",
			CallbackURL:   req.Callback,
		}

		resp := RegisterResponse{
			ClientID: clientID,
			Ack:      "registered",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// ---------------------------------------------------
// PQ Encryption Helpers
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

	ct, sharedSecret, err := scheme.Encapsulate(pubKey, rand.Reader)
	if err != nil {
		return "", err
	}

	// In production, replace XOR with an AEAD using sharedSecret
	ciphertext := make([]byte, len(message))
	for i := range message {
		ciphertext[i] = message[i] ^ sharedSecret[i%len(sharedSecret)]
	}

	combined := append(ct, ciphertext...)
	return base64.StdEncoding.EncodeToString(combined), nil
}

// ---------------------------------------------------
// Push Logic with PQ Encryption + Rotation
// ---------------------------------------------------

type ClientPushInfo struct {
	sync.Mutex
	CurrentPubKey string
	NextPubKey    string
	CallbackURL   string
}

var clientPushMap = map[string]*ClientPushInfo{}

func RotateClientKey(clientID, nextPubKey string) {
	info, ok := clientPushMap[clientID]
	if !ok {
		return
	}
	info.Lock()
	defer info.Unlock()
	info.CurrentPubKey = nextPubKey
	info.NextPubKey = ""
}

type PushPayload struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"`
	Payload string `json:"payload"` // base64(KEM ciphertext + sym ciphertext)
}

type PushResponse struct {
	NextPublicKey string `json:"next_public_key"`
}

func SendPush(clientID string, payload []byte) error {
	info, ok := clientPushMap[clientID]
	if !ok {
		return fmt.Errorf("client not found")
	}

	info.Lock()
	currentKey := info.CurrentPubKey
	callbackURL := info.CallbackURL
	info.Unlock()

	encryptedB64, err := encryptWithPQ(currentKey, payload)
	if err != nil {
		return err
	}

	pushBody := PushPayload{
		EventID: uuid.New().String(),
		Type:    "change",
		Payload: encryptedB64,
	}

	data, _ := json.Marshal(pushBody)
	req, err := http.NewRequest("POST", callbackURL, bytes.NewReader(data))
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

	body, _ := io.ReadAll(resp.Body)
	var pr PushResponse
	if err := json.Unmarshal(body, &pr); err == nil && pr.NextPublicKey != "" {
		RotateClientKey(clientID, pr.NextPublicKey)
	}

	return nil
}