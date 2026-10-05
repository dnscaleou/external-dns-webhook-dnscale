package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/dnscale"
	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
)

type Server struct {
	Provider                  *provider.Provider
	Client                    *dnscale.Client
	Timeout                   time.Duration
	MaxBodyBytes              int64
	requests, failures, nanos atomic.Uint64
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.json(w, s.Provider.Filter()) })
	mux.HandleFunc("GET /records", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.Provider.Records(r.Context())
		if err != nil {
			s.failure(w, err)
			return
		}
		s.json(w, result)
	})
	mux.HandleFunc("POST /records", func(w http.ResponseWriter, r *http.Request) {
		var changes provider.Changes
		if err := s.decode(w, r, &changes); err != nil {
			s.failure(w, err)
			return
		}
		if err := s.Provider.Apply(r.Context(), changes); err != nil {
			s.failure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /adjustendpoints", func(w http.ResponseWriter, r *http.Request) {
		var endpoints []provider.Endpoint
		if err := s.decode(w, r, &endpoints); err != nil {
			s.failure(w, err)
			return
		}
		result, err := s.Provider.Adjust(endpoints)
		if err != nil {
			s.failure(w, err)
			return
		}
		s.json(w, result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		s.requests.Add(1)
		defer func() { s.nanos.Add(uint64(time.Since(started))) }()
		timeout := s.Timeout
		if timeout == 0 {
			timeout = 90 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) error {
	limit := s.MaxBodyBytes
	if limit == 0 {
		limit = 32 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return decodeError(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return decodeError(err)
	}
	return nil
}

func decodeError(err error) error {
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		return &provider.Error{Status: 413, Message: "request body too large"}
	}
	return &provider.Error{Status: 400, Message: "invalid webhook JSON request"}
}

func (s *Server) json(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", provider.MediaType)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("webhook response interrupted")
	}
}

func (s *Server) failure(w http.ResponseWriter, err error) {
	s.failures.Add(1)
	status, message := http.StatusBadGateway, "provider operation failed"
	var failure *provider.Error
	if errors.As(err, &failure) {
		status, message = failure.Status, failure.Message
		if failure.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(failure.RetryAfter.Seconds()))))
		}
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		status, message = http.StatusGatewayTimeout, "provider request deadline or cancellation"
	}
	// Never log raw API errors, request bodies, credentials, or DNS targets.
	slog.Warn("webhook request failed", "status", status, "reason", message)
	http.Error(w, message, status)
}

func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE dnscale_webhook_requests_total counter\ndnscale_webhook_requests_total %d\n# TYPE dnscale_webhook_failures_total counter\ndnscale_webhook_failures_total %d\n# TYPE dnscale_webhook_request_duration_seconds summary\ndnscale_webhook_request_duration_seconds_sum %.9f\ndnscale_webhook_request_duration_seconds_count %d\n", s.requests.Load(), s.failures.Load(), float64(s.nanos.Load())/1e9, s.requests.Load())
		if s.Client != nil {
			fmt.Fprintf(w, "# TYPE dnscale_api_read_operations_total counter\ndnscale_api_read_operations_total %d\n# TYPE dnscale_api_writes_total counter\ndnscale_api_writes_total %d\n# TYPE dnscale_api_failures_total counter\ndnscale_api_failures_total %d\n# TYPE dnscale_api_throttles_total counter\ndnscale_api_throttles_total %d\n", s.Client.Reads.Load(), s.Client.Writes.Load(), s.Client.Failures.Load(), s.Client.Throttles.Load())
		}
	})
	return mux
}
