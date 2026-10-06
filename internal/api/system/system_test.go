package system

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/portainer/d2k/internal/types"
)

func TestVersionAdvertisesAPI144ForHealthStartInterval(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()

	h.Version(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if got := body["ApiVersion"]; got != "1.44" {
		t.Fatalf("ApiVersion = %#v, want 1.44", got)
	}
	if types.DockerAPIVersion != "1.44" {
		t.Fatalf("DockerAPIVersion = %q, want 1.44", types.DockerAPIVersion)
	}
}

func TestPingAdvertisesAPI144(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodHead, "/_ping", nil)
	rec := httptest.NewRecorder()

	h.Ping(rec, req)

	if got := rec.Header().Get("API-Version"); got != "1.44" {
		t.Fatalf("API-Version header = %q, want 1.44", got)
	}
}
