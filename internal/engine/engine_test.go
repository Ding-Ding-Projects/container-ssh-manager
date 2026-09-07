package engine

import (
	"net/http"
	"net/url"
	"testing"
)

func TestDockerRouteRejectsTraversal(t *testing.T) {
	_, _, _, err := dockerRoute(http.MethodGet, []string{"containers", "../bad"}, url.Values{})
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestDockerRouteContainerLogDefaults(t *testing.T) {
	path, method, stream, err := dockerRoute(http.MethodGet, []string{"containers", "abc", "logs"}, url.Values{})
	if err != nil || method != http.MethodGet || !stream || path != "/containers/abc/logs?stdout=true&stderr=true&tail=&timestamps=false" {
		t.Fatalf("unexpected route: %q %q %v %v", path, method, stream, err)
	}
}

func TestHostAndBodyRemovesHostID(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/?hostId=query", nil)
	host, body, err := hostAndBody(r)
	if err != nil || host != "query" || body != nil {
		t.Fatalf("unexpected empty request: %q %q %v", host, body, err)
	}
}

func TestImageURLRejected(t *testing.T) {
	if validatePayload("images", http.MethodPost, []byte(`{"url":"http://example.invalid"}`)) == nil {
		t.Fatal("expected URL rejection")
	}
}

func TestImagePullAndBuildRoutes(t *testing.T) {
	for _, operation := range []struct{ input, want string }{{"pull", "/images/create"}, {"build", "/build"}} {
		path, _, stream, err := dockerRoute(http.MethodPost, []string{"images", operation.input}, url.Values{})
		if err != nil || path != operation.want || !stream {
			t.Fatalf("%s: got %q stream=%v err=%v", operation.input, path, stream, err)
		}
	}
}
