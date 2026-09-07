package connection

import (
	"net/http/httptest"
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
