package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
)

type emptyAPI struct{ err error }

func (a emptyAPI) Zones(context.Context) ([]provider.Zone, error) {
	return []provider.Zone{{ID: "zone", Name: "example.org"}}, a.err
}
func (a emptyAPI) Records(context.Context, string) ([]provider.Record, error) {
	return []provider.Record{}, a.err
}
func (a emptyAPI) Create(context.Context, string, provider.Record) (provider.Record, error) {
	panic("unexpected write")
}
func (a emptyAPI) Update(context.Context, string, provider.Record, string) (provider.Record, error) {
	panic("unexpected write")
}
func (a emptyAPI) Delete(context.Context, string, string) error { panic("unexpected write") }
func server(t *testing.T, api emptyAPI) *Server {
	t.Helper()
	p, err := provider.New(api, provider.Options{Domains: []string{"example.org"}, ZoneIDs: []string{"zone"}, OwnerID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Provider: p}
}
func TestWireProtocol(t *testing.T) {
	s := server(t, emptyAPI{})
	h := s.Handler()
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/", "", 200}, {"GET", "/records", "", 200},
		{"POST", "/adjustendpoints", `[{"dnsName":"app.example.org","targets":["192.0.2.1"],"recordType":"A","recordTTL":0,"labels":{"resource":"service/default/app"}}]`, 200},
		{"POST", "/records", `{"create":[],"updateOld":[],"updateNew":[],"delete":[]}`, 204},
		{"POST", "/records", `{} {}`, 400}, {"POST", "/records", `{"unexpected":true}`, 400},
		{"POST", "/applychanges", `{}`, 404}, {"DELETE", "/records", "", 405},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
		if tc.status == 200 {
			if w.Header().Get("Content-Type") != provider.MediaType {
				t.Fatal("incorrect negotiation")
			}
			var v any
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
		}
		if tc.path == "/" && w.Body.String() != "{\"include\":[\"example.org\"]}\n" {
			t.Fatal("incorrect filter schema")
		}
		if tc.path == "/records" && tc.method == "GET" && w.Body.String() != "[]\n" {
			t.Fatal("empty inventory must be an array")
		}
	}
}
func TestBodyLimitAndErrorMapping(t *testing.T) {
	s := server(t, emptyAPI{})
	s.MaxBodyBytes = 8
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/records", strings.NewReader(`{"create":[]}`)))
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
	for _, status := range []int{401, 403, 409, 502, 503} {
		s = server(t, emptyAPI{&provider.Error{Status: status, Message: "sanitized"}})
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/records", nil))
		if w.Code != status {
			t.Fatal(w.Code)
		}
	}
}
func TestHealthSeparateFromAPI(t *testing.T) {
	s := server(t, emptyAPI{})
	h := httptest.NewServer(s.HealthHandler())
	defer h.Close()
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		res, err := http.Get(h.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	}
	res, err := http.Get(h.URL + "/records")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("data API exposed on health port")
	}
}
