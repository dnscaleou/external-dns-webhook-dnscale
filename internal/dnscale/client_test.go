package dnscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
)

const zoneID = "11111111-1111-4111-8111-111111111111"

func TestPaginationAndMalformedPages(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			var calls int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing authorization")
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 2 && bad {
					http.Error(w, "unavailable", 503)
					return
				}
				if calls == 1 {
					fmt.Fprint(w, `{"status":"success","data":{"zones":[{"id":"11111111-1111-4111-8111-111111111111","name":"example.org"}],"pagination":{"offset":0,"count":1,"limit":100,"total":2,"has_more":true}}}`)
				} else {
					if r.URL.Query().Get("offset") != "1" {
						t.Error("pagination offset")
					}
					fmt.Fprint(w, `{"status":"success","data":{"zones":[{"id":"22222222-2222-4222-8222-222222222222","name":"example.net"}],"pagination":{"offset":1,"count":1,"limit":100,"total":2,"has_more":false}}}`)
				}
			}))
			defer s.Close()
			c, err := New("test-token", s.URL+"/v1", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			zones, err := c.Zones(context.Background())
			if bad {
				if err == nil || zones != nil {
					t.Fatal("partial page returned as complete")
				}
			} else if err != nil || len(zones) != 2 {
				t.Fatal(zones, err)
			}
			if calls != 2 {
				t.Fatal("unexpected retry or pagination count", calls)
			}
		})
	}
}

func TestRecordWireContract(t *testing.T) {
	var methods []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		record := `{"id":"opaque-selected-value","name":"app.example.org","type":"A","content":"192.0.2.1","ttl":300,"disabled":false,"comment":"preserved","priority":null}`
		switch r.Method {
		case "GET":
			fmt.Fprintf(w, `{"status":"success","data":{"records":[%s],"pagination":{"offset":0,"count":1,"limit":100,"total":1,"has_more":false}}}`, record)
		case "POST", "PUT":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["comment"]; ok {
				t.Error("comment must be omitted")
			}
			if _, ok := body["disabled"]; ok {
				t.Error("disabled must be omitted")
			}
			if body["ttl"] != float64(300) {
				t.Error("TTL missing")
			}
			if r.Method == "POST" {
				w.WriteHeader(201)
			}
			fmt.Fprintf(w, `{"status":"success","data":{"record":%s}}`, record)
		case "DELETE":
			if !strings.HasSuffix(r.URL.Path, "/opaque-selected-value") {
				t.Error("not deleting selected ID")
			}
			w.WriteHeader(204)
		}
	}))
	defer s.Close()
	c, _ := New("test-token", s.URL+"/v1", time.Second)
	defer c.Close()
	records, err := c.Records(context.Background(), zoneID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.Create(context.Background(), zoneID, records[0])
	if err != nil {
		t.Fatal(err)
	}
	updated, err := c.Update(context.Background(), zoneID, created, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), zoneID, updated.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "GET,POST,PUT,DELETE" {
		t.Fatal(methods)
	}
}

func TestSanitizedErrorsAndCooldown(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"code":"RATE_LIMIT","message":"private-customer-data"}}`)
	}))
	defer s.Close()
	c, _ := New("test-token", s.URL, time.Second)
	defer c.Close()
	_, err := c.Zones(context.Background())
	var failure *provider.Error
	if !errors.As(err, &failure) || failure.Status != 503 || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = c.Zones(ctx); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("rate-limit cooldown ignored", err, calls.Load())
	}
}

func TestRedirectDoesNotForwardCredential(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer source.Close()
	c, _ := New("test-token", source.URL, time.Second)
	defer c.Close()
	if _, err := c.Zones(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("credential redirected")
	}
}
