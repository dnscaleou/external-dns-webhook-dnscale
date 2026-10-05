package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

type Options struct {
	Domains, ExcludeDomains, ZoneIDs []string
	OwnerID                          string
	DefaultTTL                       int
	DryRun                           bool
}

type Provider struct {
	api    API
	filter DomainFilter
	zones  map[string]bool
	owner  string
	ttl    int
	dryRun bool
	gate   chan struct{}
}

func New(api API, opts Options) (*Provider, error) {
	if api == nil || len(opts.Domains) == 0 || len(opts.ZoneIDs) == 0 {
		return nil, invalid("API client, domain filters, and zone ID filters are required")
	}
	if opts.OwnerID == "" || strings.ContainsAny(opts.OwnerID, ",=\"\\ \r\n\t") {
		return nil, invalid("an explicit TXT owner ID without delimiters is required")
	}
	if opts.DefaultTTL == 0 {
		opts.DefaultTTL = 300
	}
	if opts.DefaultTTL < 300 || opts.DefaultTTL > 86400 {
		return nil, invalid("default TTL must be between 300 and 86400 seconds")
	}
	p := &Provider{api: api, zones: make(map[string]bool), owner: opts.OwnerID, ttl: opts.DefaultTTL, dryRun: opts.DryRun, gate: make(chan struct{}, 1)}
	for _, id := range opts.ZoneIDs {
		if id == "" {
			return nil, invalid("zone ID must not be empty")
		}
		p.zones[id] = true
	}
	for _, pair := range []struct {
		input  []string
		output *[]string
	}{{opts.Domains, &p.filter.Include}, {opts.ExcludeDomains, &p.filter.Exclude}} {
		for _, value := range pair.input {
			n, err := domain(value)
			if err != nil {
				return nil, err
			}
			*pair.output = append(*pair.output, n)
		}
		slices.Sort(*pair.output)
		*pair.output = slices.Compact(*pair.output)
	}
	return p, nil
}

func (p *Provider) Filter() DomainFilter {
	return DomainFilter{slices.Clone(p.filter.Include), slices.Clone(p.filter.Exclude)}
}
func (p *Provider) lock(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *Provider) unlock() { <-p.gate }

type snapshot struct {
	zones   []Zone
	records map[string][]Record
}

func (p *Provider) snapshot(ctx context.Context) (*snapshot, error) {
	zones, err := p.api.Zones(ctx)
	if err != nil {
		return nil, err
	}
	s := &snapshot{records: make(map[string][]Record)}
	seen := make(map[string]bool)
	names := make(map[string]bool)
	for _, zone := range zones {
		zone.Name, err = domain(zone.Name)
		if err != nil || zone.ID == "" || seen[zone.ID] || names[zone.Name] {
			return nil, fmt.Errorf("ambiguous or malformed zone inventory")
		}
		seen[zone.ID], names[zone.Name] = true, true
		s.zones = append(s.zones, zone)
		if !p.zones[zone.ID] {
			continue
		}
		records, err := p.api.Records(ctx, zone.ID)
		if err != nil {
			return nil, err
		}
		for i := range records {
			r := &records[i]
			r.Name, err = domain(r.Name)
			if err != nil || !under(r.Name, zone.Name) || r.ID == "" {
				return nil, fmt.Errorf("malformed record inventory")
			}
			r.Type = strings.ToUpper(r.Type)
			if supported(r.Type) {
				r.Content, err = target(r.Type, r.Content, false)
				if err != nil {
					return nil, fmt.Errorf("malformed record target")
				}
			}
		}
		s.records[zone.ID] = records
	}
	for id := range p.zones {
		if !seen[id] {
			return nil, &Error{Status: 403, Message: "configured zone is absent or inaccessible"}
		}
	}
	slices.SortFunc(s.zones, func(a, b Zone) int { return len(b.Name) - len(a.Name) })
	return s, nil
}

func (p *Provider) zone(s *snapshot, name string) (Zone, error) {
	if !p.allowed(name) {
		return Zone{}, &Error{Status: 403, Message: "record is outside configured domains"}
	}
	for _, zone := range s.zones {
		if under(name, zone.Name) {
			if !p.zones[zone.ID] {
				return Zone{}, &Error{Status: 403, Message: "more specific zone is outside the zone allowlist"}
			}
			return zone, nil
		}
	}
	return Zone{}, &Error{Status: 403, Message: "no authorized zone for record"}
}

func (p *Provider) Records(ctx context.Context) ([]Endpoint, error) {
	if err := p.lock(ctx); err != nil {
		return nil, err
	}
	defer p.unlock()
	s, err := p.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	endpoints := make(map[string]Endpoint)
	for zoneID, records := range s.records {
		for _, r := range records {
			if !supported(r.Type) || !p.allowed(r.Name) {
				continue
			}
			z, err := p.zone(s, r.Name)
			if err != nil || z.ID != zoneID {
				continue
			}
			if r.Disabled {
				return nil, conflict("disabled records exist in the managed scope")
			}
			k := key(r.Name, r.Type)
			ep, ok := endpoints[k]
			if ok && ep.RecordTTL != int64(r.TTL) {
				return nil, conflict("inconsistent TTLs in a record set")
			}
			if !ok {
				ep = Endpoint{DNSName: r.Name, RecordType: r.Type, RecordTTL: int64(r.TTL)}
			}
			ep.Targets = append(ep.Targets, r.Content)
			endpoints[k] = ep
		}
	}
	keys := make([]string, 0, len(endpoints))
	for k := range endpoints {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	result := make([]Endpoint, 0, len(keys))
	for _, k := range keys {
		ep := endpoints[k]
		slices.Sort(ep.Targets)
		ep.Targets = slices.Compact(ep.Targets)
		result = append(result, wire(ep))
	}
	return result, nil
}
