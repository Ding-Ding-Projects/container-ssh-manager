package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShellQuoteContainsSingleOperand(t *testing.T) {
	got := shellQuote("/srv/a'b")
	if got != "'/srv/a'\\''b'" {
		t.Fatalf("unexpected quote: %q", got)
	}
}

func TestComposeDeployUsesOnlyControlledFlags(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := composeCommand("/srv/app", "deploy", r); got != "" {
		t.Fatalf("invalid JSON should reject command: %q", got)
	}
}

func TestComposePathValidation(t *testing.T) {
	if safeComposePath("relative/path") || safeComposePath("C:\\") || !safeComposePath("C:\\srv\\app") {
		t.Fatal("unexpected path validation result")
	}
}
