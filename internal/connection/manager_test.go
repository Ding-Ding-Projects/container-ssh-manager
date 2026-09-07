package connection

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeRemotePath(t *testing.T) {
	for _, p := range []string{"", "relative", "/bad\x00path"} {
		if err := safeRemotePath(p); err == nil {
			t.Fatalf("%q accepted", p)
		}
	}
	if err := safeRemotePath("/var/log/app.log"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteComposeFileLocal(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{}
	if err := m.WriteComposeFile(context.Background(), "local", dir, "compose.yaml", []byte("services: {}\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "services: {}\n" {
		t.Fatalf("got %q", b)
	}
	if err := m.WriteComposeFile(context.Background(), "local", dir, "bad.yaml", []byte("x")); err == nil {
		t.Fatal("unsafe basename accepted")
	}
}

func TestHashTextStableAndDistinct(t *testing.T) {
	if HashText([]byte("a")) != HashText([]byte("a")) {
		t.Fatal("hash is not stable")
	}
	if HashText([]byte("a")) == HashText([]byte("b")) {
		t.Fatal("hash collision")
	}
}

func TestSameOriginRequiresExactHost(t *testing.T) {
	r := httptest.NewRequest("GET", "http://manager.test/x", nil)
	r.Host = "manager.test"
	r.Header.Set("Origin", "https://manager.test.attacker.example")
	if sameOrigin(r) {
		t.Fatal("substring origin accepted")
	}
	r.Header.Set("Origin", "https://manager.test")
	if !sameOrigin(r) {
		t.Fatal("exact origin rejected")
	}
}
