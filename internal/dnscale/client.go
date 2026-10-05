// Package dnscale isolates SDK types from the webhook reconciliation engine.
package dnscale

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	sdk "github.com/dnscaleou/dnscale-go"
	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
)

type Client struct {
	sdk                                *sdk.Client
	notBefore                          atomic.Int64
	Reads, Writes, Failures, Throttles atomic.Uint64
}

func New(token, baseURL string, timeout time.Duration) (*Client, error) {
	c, err := sdk.New(sdk.Options{APIKey: token, BaseURL: baseURL, Timeout: timeout, MaxRetries: sdk.Ptr(0)})
	if err != nil {
		return nil, err
	}
	return &Client{sdk: c}, nil
}

func (c *Client) Close() { c.sdk.Close() }

// API response text may contain customer data. Return only stable, sanitized
// errors, while retaining retryability. Controller retries re-read all state.
func (c *Client) result(err error) error {
	if err == nil {
		return nil
	}
	c.Failures.Add(1)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401, 403:
			return &provider.Error{Status: apiErr.StatusCode, Message: "DNScale API authentication or authorization failed"}
		case 429:
			c.Throttles.Add(1)
			c.notBefore.Store(time.Now().Add(retryAfter(err)).UnixNano())
			return &provider.Error{Status: 503, Message: "DNScale API rate limit exceeded", RetryAfter: retryAfter(err)}
		case 400, 404, 409, 422:
			return &provider.Error{Status: 409, Message: "DNScale API rejected the operation; check record state and token scope"}
		}
	}
	return &provider.Error{Status: 502, Message: "DNScale API request failed"}
}

func retryAfter(err error) time.Duration {
	var limited *sdk.RateLimitError
	if !errors.As(err, &limited) {
		return time.Second
	}
	if seconds, e := time.ParseDuration(limited.RetryAfter + "s"); e == nil && seconds > 0 {
		return min(seconds, 5*time.Minute)
	}
	if when, e := http.ParseTime(limited.RetryAfter); e == nil {
		return max(time.Second, min(time.Until(when), 5*time.Minute))
	}
	return time.Second
}

func (c *Client) Zones(ctx context.Context) ([]provider.Zone, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	c.Reads.Add(1)
	result := make([]provider.Zone, 0)
	for z, err := range c.sdk.Zones.Iter(ctx, 100) {
		if err != nil {
			return nil, c.result(err)
		}
		result = append(result, provider.Zone{ID: z.Id.String(), Name: z.Name})
	}
	return result, nil
}

func fromRecord(r sdk.Record) provider.Record {
	return provider.Record{ID: r.Id, Name: r.Name, Type: string(r.Type), Content: r.Content, TTL: r.Ttl, Disabled: r.Disabled}
}

func (c *Client) Records(ctx context.Context, zone string) ([]provider.Record, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	c.Reads.Add(1)
	result := make([]provider.Record, 0)
	for r, err := range c.sdk.Records.Iter(ctx, zone, 100) {
		if err != nil {
			return nil, c.result(err)
		}
		result = append(result, fromRecord(r))
	}
	return result, nil
}

func request(r provider.Record) sdk.CreateRecordRequest {
	return sdk.CreateRecordRequest{Name: r.Name, Type: sdk.RecordType(r.Type), Content: r.Content, Ttl: sdk.Ptr(r.TTL)}
}

func (c *Client) Create(ctx context.Context, zone string, r provider.Record) (provider.Record, error) {
	if err := c.wait(ctx); err != nil {
		return provider.Record{}, err
	}
	result, err := c.sdk.Records.Create(ctx, zone, request(r))
	if err != nil {
		return provider.Record{}, c.result(err)
	}
	c.Writes.Add(1)
	return fromRecord(*result), nil
}

func (c *Client) Update(ctx context.Context, zone string, r provider.Record, id string) (provider.Record, error) {
	if err := c.wait(ctx); err != nil {
		return provider.Record{}, err
	}
	result, err := c.sdk.Records.Update(ctx, zone, id, request(r))
	if err != nil {
		return provider.Record{}, c.result(err)
	}
	c.Writes.Add(1)
	return fromRecord(*result), nil
}

func (c *Client) Delete(ctx context.Context, zone, id string) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	if err := c.sdk.Records.Delete(ctx, zone, id); err != nil {
		return c.result(err)
	}
	c.Writes.Add(1)
	return nil
}

func (c *Client) wait(ctx context.Context) error {
	delay := time.Until(time.Unix(0, c.notBefore.Load()))
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
