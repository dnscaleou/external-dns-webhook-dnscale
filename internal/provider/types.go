// Package provider translates the ExternalDNS webhook wire protocol to DNScale.
package provider

import (
	"context"
	"time"
)

const MediaType = "application/external.dns.webhook+json;version=1"

// These wire fields follow ExternalDNS v0.23.0's endpoint and plan schemas.
// The adapter deliberately does not import its Kubernetes dependency tree.
type Endpoint struct {
	DNSName          string            `json:"dnsName,omitempty"`
	Targets          []string          `json:"targets,omitempty"`
	RecordType       string            `json:"recordType,omitempty"`
	SetIdentifier    string            `json:"setIdentifier,omitempty"`
	RecordTTL        int64             `json:"recordTTL,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	ProviderSpecific []Property        `json:"providerSpecific,omitempty"`
}

type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Changes struct {
	Create    []Endpoint `json:"create,omitempty"`
	UpdateOld []Endpoint `json:"updateOld,omitempty"`
	UpdateNew []Endpoint `json:"updateNew,omitempty"`
	Delete    []Endpoint `json:"delete,omitempty"`
}

type DomainFilter struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

type Zone struct{ ID, Name string }
type Record struct {
	ID, Name, Type, Content string
	TTL                     int
	Disabled                bool
}

type API interface {
	Zones(context.Context) ([]Zone, error)
	Records(context.Context, string) ([]Record, error)
	Create(context.Context, string, Record) (Record, error)
	Update(context.Context, string, Record, string) (Record, error)
	Delete(context.Context, string, string) error
}

type Error struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string      { return e.Message }
func invalid(message string) error  { return &Error{Status: 400, Message: message} }
func conflict(message string) error { return &Error{Status: 409, Message: message} }
