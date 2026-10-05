package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const testZone = "11111111-1111-4111-8111-111111111111"

type memoryAPI struct {
	zones                 []Zone
	records               map[string][]Record
	writes, reads, failAt int
	failAfter             bool
	readError             error
}

func fresh(t *testing.T) (*Provider, *memoryAPI) {
	t.Helper()
	a := &memoryAPI{zones: []Zone{{testZone, "example.org"}}, records: map[string][]Record{testZone: {}}}
	p, err := New(a, Options{Domains: []string{"example.org"}, ZoneIDs: []string{testZone}, OwnerID: "test-owner"})
	if err != nil {
		t.Fatal(err)
	}
	return p, a
}
func (a *memoryAPI) Zones(context.Context) ([]Zone, error) { a.reads++; return a.zones, a.readError }
func (a *memoryAPI) Records(_ context.Context, z string) ([]Record, error) {
	a.reads++
	return slices.Clone(a.records[z]), a.readError
}
func (a *memoryAPI) mutate(fn func()) error {
	a.writes++
	fail := a.failAt == a.writes
	if fail && !a.failAfter {
		return errors.New("injected failure")
	}
	fn()
	if fail {
		return errors.New("response lost after commit")
	}
	return nil
}
func (a *memoryAPI) Create(_ context.Context, z string, r Record) (Record, error) {
	r.ID = r.Name + "|" + r.Type + "|" + r.Content
	err := a.mutate(func() {
		for _, old := range a.records[z] {
			if old.Name == r.Name && (old.Type == "CNAME" || r.Type == "CNAME") {
				panic("invalid CNAME create ordering")
			}
		}
		for i := range a.records[z] {
			if a.records[z][i].Name == r.Name && a.records[z][i].Type == r.Type {
				a.records[z][i].TTL = r.TTL
			}
		}
		a.records[z] = append(a.records[z], r)
	})
	return r, err
}
func (a *memoryAPI) Update(_ context.Context, z string, r Record, id string) (Record, error) {
	r.ID = r.Name + "|" + r.Type + "|" + r.Content
	err := a.mutate(func() {
		for i, old := range a.records[z] {
			if old.ID == id {
				a.records[z][i] = r
			}
			if old.Name == r.Name && old.Type == r.Type {
				a.records[z][i].TTL = r.TTL
			}
		}
	})
	return r, err
}
func (a *memoryAPI) Delete(_ context.Context, z, id string) error {
	return a.mutate(func() { a.records[z] = slices.DeleteFunc(a.records[z], func(r Record) bool { return r.ID == id }) })
}

func data(typ string, targets ...string) Endpoint {
	return Endpoint{DNSName: "app.example.org", RecordType: typ, RecordTTL: 300, Targets: targets, Labels: map[string]string{"owner": "test-owner", "resource": "service/default/app"}}
}
func ownership(ep Endpoint) Endpoint {
	return Endpoint{DNSName: marker(ep.DNSName, ep.RecordType), RecordType: "TXT", Targets: []string{`"heritage=external-dns,external-dns/owner=test-owner,external-dns/resource=service/default/app"`}}
}
func pair(ep Endpoint) []Endpoint { return []Endpoint{ep, ownership(ep)} }
func assertApply(t *testing.T, p *Provider, c Changes) {
	t.Helper()
	if err := p.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleAndReplay(t *testing.T) {
	for _, typ := range []string{"A", "AAAA", "CNAME"} {
		t.Run(typ, func(t *testing.T) {
			p, a := fresh(t)
			initial, updated := data(typ, "192.0.2.1", "192.0.2.2"), data(typ, "192.0.2.2", "192.0.2.3")
			if typ == "AAAA" {
				initial.Targets = []string{"2001:db8::1", "2001:db8::2"}
				updated.Targets = []string{"2001:db8::2", "2001:db8::3"}
			}
			if typ == "CNAME" {
				initial.Targets = []string{"old.example.net"}
				updated.Targets = []string{"new.example.net"}
			}
			for _, c := range []Changes{{Create: pair(initial)}, {UpdateOld: pair(initial), UpdateNew: pair(updated)}, {Delete: pair(updated)}} {
				assertApply(t, p, c)
				before := a.writes
				assertApply(t, p, c)
				if a.writes != before {
					t.Fatal("replay wrote records")
				}
			}
			if len(a.records[testZone]) != 0 {
				t.Fatal("cleanup left records")
			}
		})
	}
}

func TestRecoveryAfterEveryMutation(t *testing.T) {
	initial, newData := data("A", "192.0.2.1", "192.0.2.2"), data("CNAME", "lb.example.net")
	scenarios := []struct {
		name          string
		setup, change Changes
	}{
		{"create", Changes{}, Changes{Create: pair(initial)}},
		{"replace", Changes{Create: pair(initial)}, Changes{UpdateOld: pair(initial), UpdateNew: pair(data("A", "192.0.2.2", "192.0.2.3"))}},
		{"transition", Changes{Create: pair(initial)}, Changes{Delete: pair(initial), Create: pair(newData)}},
		{"delete", Changes{Create: pair(initial)}, Changes{Delete: pair(initial)}},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			want, control := fresh(t)
			assertApply(t, want, scenario.setup)
			start := control.writes
			assertApply(t, want, scenario.change)
			count := control.writes - start
			for n := 1; n <= count; n++ {
				for _, after := range []bool{false, true} {
					t.Run(fmt.Sprintf("%d/after=%t", n, after), func(t *testing.T) {
						p, a := fresh(t)
						assertApply(t, p, scenario.setup)
						a.failAt = a.writes + n
						a.failAfter = after
						_ = p.Apply(context.Background(), scenario.change)
						a.failAt = 0
						// Recover with a new provider process, not cached record IDs.
						p, err := New(a, Options{Domains: []string{"example.org"}, ZoneIDs: []string{testZone}, OwnerID: "test-owner"})
						if err != nil {
							t.Fatal(err)
						}
						assertApply(t, p, scenario.change)
						got, _ := p.Records(context.Background())
						expected, _ := want.Records(context.Background())
						if !reflect.DeepEqual(got, expected) {
							t.Fatalf("recovery differs: %#v != %#v", got, expected)
						}
					})
				}
			}
		})
	}
}

func TestTTLUpdateAndMetadataChange(t *testing.T) {
	p, a := fresh(t)
	ep := data("A", "192.0.2.1", "192.0.2.2")
	assertApply(t, p, Changes{Create: pair(ep)})
	updated := ep
	updated.RecordTTL = 600
	oldPair, newPair := pair(ep), pair(updated)
	newPair[1].Targets = []string{`"heritage=external-dns,external-dns/owner=test-owner,external-dns/resource=service/default/renamed"`}
	assertApply(t, p, Changes{UpdateOld: oldPair, UpdateNew: newPair})
	for _, r := range a.records[testZone] {
		if r.Type == "A" && r.TTL != 600 {
			t.Fatal("TTL not changed across RRset")
		}
		if r.Type == "TXT" && !strings.Contains(r.Content, "renamed") {
			t.Fatal("old metadata remains")
		}
	}
	before := a.writes
	assertApply(t, p, Changes{UpdateOld: oldPair, UpdateNew: newPair})
	if before != a.writes {
		t.Fatal("TTL replay wrote")
	}
}

func TestRefusesUnownedForeignDisabledOrChangedRecords(t *testing.T) {
	for _, kind := range []string{"unowned", "foreign", "disabled", "changed", "foreign-marker", "unrelated-marker"} {
		t.Run(kind, func(t *testing.T) {
			p, a := fresh(t)
			ep := data("A", "192.0.2.1")
			assertApply(t, p, Changes{Create: pair(ep)})
			for i := range a.records[testZone] {
				r := &a.records[testZone][i]
				if r.Type == "TXT" {
					switch kind {
					case "unowned":
						r.Name = "manual.example.org"
					case "foreign", "foreign-marker":
						r.Content = strings.ReplaceAll(r.Content, "test-owner", "other-owner")
					case "unrelated-marker":
						r.Content = "unrelated"
					}
				}
				if r.Type == "A" {
					switch kind {
					case "disabled":
						r.Disabled = true
					case "changed":
						r.Content = "192.0.2.99"
					}
				}
			}
			before := a.writes
			err := p.Apply(context.Background(), Changes{UpdateOld: pair(ep), UpdateNew: pair(data("A", "192.0.2.2"))})
			if err == nil || a.writes != before {
				t.Fatal("unsafe mutation accepted")
			}
		})
	}
}

func TestWholeBatchPreflight(t *testing.T) {
	for _, mutate := range []func(*Endpoint){
		func(e *Endpoint) { e.DNSName = "outside.invalid" }, func(e *Endpoint) { e.RecordTTL = 60 }, func(e *Endpoint) { e.RecordType = "MX" }, func(e *Endpoint) { e.Targets = []string{"not-an-ip"} }, func(e *Endpoint) { e.SetIdentifier = "weighted" }, func(e *Endpoint) { e.ProviderSpecific = []Property{{"weight", "1"}} },
	} {
		p, a := fresh(t)
		bad := data("A", "192.0.2.5")
		bad.DNSName = "bad.example.org"
		mutate(&bad)
		c := Changes{Create: append(pair(data("A", "192.0.2.1")), bad)}
		if p.Apply(context.Background(), c) == nil || a.writes != 0 {
			t.Fatal("preflight allowed partial writes")
		}
	}
}

func TestDiscoveryFiltersAndNestedZones(t *testing.T) {
	p, a := fresh(t)
	a.zones = append(a.zones, Zone{"child", "child.example.org"})
	ep := data("A", "192.0.2.1")
	ep.DNSName = "app.child.example.org"
	if p.Apply(context.Background(), Changes{Create: pair(ep)}) == nil {
		t.Fatal("fell back to parent zone")
	}
	p.filter.Exclude = []string{"private.example.org"}
	for _, name := range []string{"app.private.example.org", "notexample.org"} {
		ep.DNSName = name
		if p.Apply(context.Background(), Changes{Create: pair(ep)}) == nil {
			t.Fatal("filter bypass")
		}
	}
	p.zones["missing"] = true
	if _, err := p.Records(context.Background()); err == nil {
		t.Fatal("missing zone treated as empty")
	}
}

func TestReadFailureAndCancellationNeverWrite(t *testing.T) {
	p, a := fresh(t)
	a.readError = errors.New("page failed")
	if _, err := p.Records(context.Background()); err == nil {
		t.Fatal("partial inventory accepted")
	}
	if p.Apply(context.Background(), Changes{Create: pair(data("A", "192.0.2.1"))}) == nil || a.writes != 0 {
		t.Fatal("wrote after read error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.readError = nil
	if p.Apply(ctx, Changes{Create: pair(data("A", "192.0.2.1"))}) == nil || a.writes != 0 {
		t.Fatal("wrote after cancellation")
	}
}

func TestAdjustNormalizationAndTXT(t *testing.T) {
	p, _ := fresh(t)
	ep := data("AAAA", "2001:DB8:0::1", "2001:db8::1")
	ep.DNSName = "APP.EXAMPLE.ORG."
	ep.RecordTTL = 0
	got, err := p.Adjust([]Endpoint{ep})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].DNSName != "app.example.org" || got[0].RecordTTL != 300 || !reflect.DeepEqual(got[0].Targets, []string{"2001:db8::1"}) || !reflect.DeepEqual(got[0].Labels, ep.Labels) {
		t.Fatalf("bad normalization: %#v", got)
	}
	txt := ownership(data("A", "192.0.2.1"))
	got, err = p.Adjust([]Endpoint{txt})
	if err != nil || !reflect.DeepEqual(got[0].Targets, txt.Targets) {
		t.Fatal("TXT quoting changed")
	}
	ep.DNSName = "*.example.org"
	if _, err = p.Adjust([]Endpoint{ep}); err == nil {
		t.Fatal("wildcard accepted without tested mapper")
	}
}

func TestManualAndCertManagerRecordsSurvive(t *testing.T) {
	p, a := fresh(t)
	untouched := []Record{{ID: "manual", Name: "manual.example.org", Type: "A", Content: "192.0.2.90", TTL: 3600}, {ID: "acme", Name: "_acme-challenge.example.org", Type: "TXT", Content: "acme-challenge-value", TTL: 300}}
	a.records[testZone] = slices.Clone(untouched)
	ep := data("A", "192.0.2.1")
	assertApply(t, p, Changes{Create: pair(ep)})
	assertApply(t, p, Changes{Delete: pair(ep)})
	if !reflect.DeepEqual(a.records[testZone], untouched) {
		t.Fatal("unrelated records changed")
	}
}

func TestDryRunPreflightsWithoutWriting(t *testing.T) {
	p, a := fresh(t)
	p.dryRun = true
	assertApply(t, p, Changes{Create: pair(data("A", "192.0.2.1"))})
	if a.writes != 0 || a.reads == 0 {
		t.Fatal("dry-run must read and validate without writes")
	}
	ep := data("A", "192.0.2.1")
	ep.DNSName = "outside.invalid"
	if p.Apply(context.Background(), Changes{Create: pair(ep)}) == nil {
		t.Fatal("dry-run skipped validation")
	}
}
