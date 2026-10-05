package provider

import (
	"context"
	"log/slog"
	"slices"
)

type delta struct {
	old, new *Endpoint
	zone     Zone
	current  []Record
}

func (d *delta) endpoint() *Endpoint {
	if d.new != nil {
		return d.new
	}
	return d.old
}
func contains(ep *Endpoint, value string) bool {
	return ep != nil && slices.Contains(ep.Targets, value)
}

// Apply preflights the entire batch before writing. Each request starts with a
// fresh inventory, including after an uncertain response or controller retry.
func (p *Provider) Apply(ctx context.Context, changes Changes) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	deltas := make(map[string]*delta)
	for _, group := range []struct {
		eps     []Endpoint
		desired bool
	}{
		{changes.Delete, false}, {changes.UpdateOld, false}, {changes.Create, true}, {changes.UpdateNew, true},
	} {
		for _, raw := range group.eps {
			ep, err := p.normalize(raw, group.desired)
			if err != nil {
				return err
			}
			if v := ep.Labels["owner"]; v != "" && v != p.owner {
				return conflict("endpoint belongs to another owner")
			}
			k := key(ep.DNSName, ep.RecordType)
			d := deltas[k]
			if d == nil {
				d = &delta{}
				deltas[k] = d
			}
			slot := &d.old
			if group.desired {
				slot = &d.new
			}
			if *slot != nil {
				return invalid("duplicate record set in change batch")
			}
			*slot = &ep
		}
	}
	if len(deltas) == 0 {
		return nil
	}
	s, err := p.snapshot(ctx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(deltas))
	for k, d := range deltas {
		keys = append(keys, k)
		ep := d.endpoint()
		d.zone, err = p.zone(s, ep.DNSName)
		if err != nil {
			return err
		}
		for _, r := range s.records[d.zone.ID] {
			if r.Name != ep.DNSName || r.Type != ep.RecordType {
				continue
			}
			if r.Disabled {
				return conflict("disabled record cannot be managed")
			}
			if !contains(d.old, r.Content) && !contains(d.new, r.Content) {
				return conflict("record set changed since planning or contains unrelated values")
			}
			d.current = append(d.current, r)
		}
	}
	slices.Sort(keys)
	// Verify existing ownership, and ensure data and ownership cannot be detached
	// by a malformed batch. TXT markers are dedicated to one owner and RRset.
	for _, k := range keys {
		d := deltas[k]
		ep := d.endpoint()
		if ep.RecordType == "TXT" {
			base, typ, _ := markerBase(ep.DNSName)
			z, err := p.zone(s, base)
			if err != nil {
				return err
			}
			if z.ID != d.zone.ID {
				return conflict("ownership marker crosses a zone boundary")
			}
			data := deltas[key(base, typ)]
			if data == nil {
				return invalid("ownership change must accompany its data record set")
			}
			if d.new == nil && data.new != nil {
				return invalid("cannot remove ownership from retained data")
			}
			if d.new != nil && data.new == nil {
				return invalid("cannot retain ownership for deleted data")
			}
			continue
		}
		markerName := marker(ep.DNSName, ep.RecordType)
		if _, err := domain(markerName); err != nil {
			return invalid("ownership name exceeds DNS limits")
		}
		z, err := p.zone(s, markerName)
		if err != nil {
			return err
		}
		if z.ID != d.zone.ID {
			return conflict("ownership marker crosses a zone boundary")
		}
		owned := false
		for _, r := range s.records[d.zone.ID] {
			if r.Name != markerName || r.Type != "TXT" {
				continue
			}
			if r.Disabled || owner(r.Content) != p.owner {
				return conflict("ownership marker is disabled or belongs to another writer")
			}
			owned = true
		}
		md := deltas[key(markerName, "TXT")]
		if len(d.current) > 0 && !owned {
			return conflict("refusing to adopt an existing unowned record set")
		}
		if d.new != nil && !owned && (md == nil || md.new == nil) {
			return invalid("data creation requires its ownership marker")
		}
		// A replay of a completed deletion may find both data and marker absent.
		if d.new == nil && md == nil {
			return invalid("data deletion requires its ownership marker deletion")
		}
	}
	// Check the final DNS state for CNAME exclusivity before the first mutation.
	for _, k := range keys {
		d := deltas[k]
		ep := d.endpoint()
		if d.new == nil {
			continue
		}
		for _, r := range s.records[d.zone.ID] {
			if r.Name != ep.DNSName || r.Type == ep.RecordType {
				continue
			}
			if ep.RecordType != "CNAME" && r.Type != "CNAME" {
				continue
			}
			other := deltas[key(r.Name, r.Type)]
			if other == nil || other.new != nil {
				return conflict("CNAME cannot coexist with another record type")
			}
		}
		for _, other := range deltas {
			if other.new != nil && other.new.DNSName == ep.DNSName && other.new.RecordType != ep.RecordType && (ep.RecordType == "CNAME" || other.new.RecordType == "CNAME") {
				return conflict("CNAME cannot coexist with another record type")
			}
		}
	}
	// ExternalDNS's webhook wire protocol does not carry its dry-run flag.
	// The sidecar must enforce this independently after validating the batch.
	if p.dryRun {
		slog.Info("dry-run batch validated", "record_sets", len(deltas))
		return nil
	}
	// Establish ownership first. A failure after this point is recoverable by a
	// new process: TXT remains until its data has been deleted successfully.
	for _, k := range keys {
		d := deltas[k]
		if d.endpoint().RecordType == "TXT" && d.new != nil {
			if err := p.upsert(ctx, d); err != nil {
				return err
			}
		}
	}
	// Pure deletions precede additions so A/AAAA <-> CNAME conversions work.
	for _, k := range keys {
		d := deltas[k]
		if d.endpoint().RecordType != "TXT" && d.new == nil {
			if err := p.remove(ctx, d); err != nil {
				return err
			}
		}
	}
	for _, k := range keys {
		d := deltas[k]
		if d.endpoint().RecordType != "TXT" && d.new != nil {
			if err := p.upsert(ctx, d); err != nil {
				return err
			}
			if err := p.remove(ctx, d); err != nil {
				return err
			}
		}
	}
	for _, k := range keys {
		d := deltas[k]
		if d.endpoint().RecordType == "TXT" {
			if err := p.remove(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Provider) upsert(ctx context.Context, d *delta) error {
	ep := d.new
	if err := ctx.Err(); err != nil {
		return err
	}
	if ep.RecordType == "CNAME" && len(d.current) > 0 {
		if len(d.current) != 1 {
			return conflict("invalid CNAME record set")
		}
		r := d.current[0]
		if r.Content != ep.Targets[0] || r.TTL != int(ep.RecordTTL) {
			r.Content, r.TTL = ep.Targets[0], int(ep.RecordTTL)
			updated, err := p.api.Update(ctx, d.zone.ID, r, r.ID)
			if err != nil {
				return err
			}
			updated, err = validateWrite(updated, r)
			if err != nil {
				return err
			}
			d.current = []Record{updated}
		}
		return nil
	}
	for _, value := range ep.Targets {
		found := false
		for _, r := range d.current {
			if r.Content == value {
				found = true
				break
			}
		}
		if found {
			continue
		}
		r := Record{Name: ep.DNSName, Type: ep.RecordType, Content: value, TTL: int(ep.RecordTTL)}
		created, err := p.api.Create(ctx, d.zone.ID, r)
		if err != nil {
			return err
		}
		created, err = validateWrite(created, r)
		if err != nil {
			return err
		}
		d.current = append(d.current, created)
	}
	// TTL applies to the entire RRset. Update a retained value once; the API
	// preserves comments when the optional comment field is omitted.
	for _, r := range d.current {
		if contains(ep, r.Content) && r.TTL != int(ep.RecordTTL) {
			r.TTL = int(ep.RecordTTL)
			if _, err := p.api.Update(ctx, d.zone.ID, r, r.ID); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func validateWrite(got, want Record) (Record, error) {
	name, nameErr := domain(got.Name)
	content, contentErr := target(want.Type, got.Content, false)
	if nameErr != nil || contentErr != nil || got.ID == "" || name != want.Name || got.Type != want.Type || content != want.Content || got.TTL != want.TTL || got.Disabled {
		return Record{}, &Error{Status: 502, Message: "DNScale API returned an unexpected record after a write"}
	}
	got.Name, got.Content = name, content
	return got, nil
}

func (p *Provider) remove(ctx context.Context, d *delta) error {
	for _, r := range d.current {
		if contains(d.new, r.Content) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.api.Delete(ctx, d.zone.ID, r.ID); err != nil {
			// A backend can commit a delete and then lose its response. Confirm
			// absence using a complete fresh read; never fall back to RRset delete.
			records, readErr := p.api.Records(ctx, d.zone.ID)
			if readErr != nil {
				return err
			}
			for _, current := range records {
				if current.ID == r.ID || current.Name == r.Name && current.Type == r.Type && current.Content == r.Content {
					return err
				}
			}
		}
	}
	return nil
}
