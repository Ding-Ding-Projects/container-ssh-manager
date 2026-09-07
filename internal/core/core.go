package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct{ DB *sql.DB }

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,data TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(kind,id));
 CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL);
 INSERT OR IGNORE INTO schema_migrations VALUES(1,strftime('%Y-%m-%dT%H:%M:%fZ','now'));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Put(kind, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec(`INSERT INTO records(kind,id,data,updated_at) VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, kind, id, string(b), time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) Get(kind, id string, v any) error {
	var b string
	if e := s.DB.QueryRow(`SELECT data FROM records WHERE kind=? AND id=?`, kind, id).Scan(&b); e != nil {
		return e
	}
	return json.Unmarshal([]byte(b), v)
}
func (s *Store) List(kind string) ([]json.RawMessage, error) {
	rows, e := s.DB.Query(`SELECT data FROM records WHERE kind=? ORDER BY id`, kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
func (s *Store) Delete(kind, id string) error {
	_, e := s.DB.Exec(`DELETE FROM records WHERE kind=? AND id=?`, kind, id)
	return e
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Backup(path string) error {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return errors.New("backup destination must not exist")
	}
	_, err := s.DB.Exec(`VACUUM INTO '` + strings.ReplaceAll(path, "'", "''") + `'`)
	if err == nil {
		err = os.Chmod(path, 0600)
	}
	return err
}

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("vault key must contain exactly 32 bytes")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(block)
	return &Vault{a}, e
}
func LoadVault(path string) (*Vault, error) {
	key, e := os.ReadFile(path)
	if e != nil {
		return nil, fmt.Errorf("vault key unavailable: %w", e)
	}
	defer clear(key)
	return NewVault(key)
}
func (v *Vault) Seal(p []byte) ([]byte, error) {
	n := make([]byte, v.aead.NonceSize())
	if _, e := rand.Read(n); e != nil {
		return nil, e
	}
	return v.aead.Seal(n, n, p, []byte("container-ssh-manager:v1")), nil
}
func (v *Vault) Open(b []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(b) < n {
		return nil, errors.New("invalid encrypted record")
	}
	return v.aead.Open(nil, b[:n], b[n:], []byte("container-ssh-manager:v1"))
}
func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]string{"error": message})
}
func Decode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if e := dec.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}
