package core

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestVaultAuthenticatedEncryption(t *testing.T) {
	v, _ := NewVault(bytes.Repeat([]byte{3}, 32))
	sealed, e := v.Seal([]byte("fixture-value"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(sealed, []byte("fixture-value")) {
		t.Fatal("plaintext leaked")
	}
	plain, e := v.Open(sealed)
	if e != nil || string(plain) != "fixture-value" {
		t.Fatal("roundtrip")
	}
	sealed[len(sealed)-1] ^= 1
	if _, e = v.Open(sealed); e == nil {
		t.Fatal("tamper accepted")
	}
	other, _ := NewVault(bytes.Repeat([]byte{4}, 32))
	if _, e = other.Open(sealed); e == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestStoreBackupRestore(t *testing.T) {
	dir := t.TempDir()
	s, e := NewStore(filepath.Join(dir, "main.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Put("hosts", "one", map[string]string{"name": "fixture"}); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "backup.db")
	if e = s.Backup(path); e != nil {
		t.Fatal(e)
	}
	b, e := NewStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	var h map[string]string
	if e = b.Get("hosts", "one", &h); e != nil || h["name"] != "fixture" {
		t.Fatal("restore failed", e)
	}
	if e = s.Backup(path); e == nil {
		t.Fatal("overwrote backup")
	}
}
