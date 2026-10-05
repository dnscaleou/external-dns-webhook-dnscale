package provider

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

func domain(name string) (string, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if len(name) == 0 || len(name) > 253 {
		return "", invalid("invalid DNS name")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalid("invalid DNS label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", invalid("DNS names must be ASCII without wildcards")
			}
		}
	}
	return name, nil
}

func under(name, suffix string) bool { return name == suffix || strings.HasSuffix(name, "."+suffix) }
func supported(typ string) bool      { return typ == "A" || typ == "AAAA" || typ == "CNAME" || typ == "TXT" }
func key(name, typ string) string    { return name + "\x00" + typ }
func marker(name, typ string) string { return "edns-" + strings.ToLower(typ) + "." + name }

func (p *Provider) allowed(name string) bool {
	for _, excluded := range p.filter.Exclude {
		if under(name, excluded) {
			return false
		}
	}
	for _, included := range p.filter.Include {
		if under(name, included) {
			return true
		}
	}
	return false
}

func target(typ, value string, fromWire bool) (string, error) {
	switch typ {
	case "A", "AAAA":
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" || (typ == "A" && !ip.Is4()) || (typ == "AAAA" && (!ip.Is6() || ip.Is4In6())) {
			return "", invalid("invalid IP target")
		}
		return ip.String(), nil
	case "CNAME":
		return domain(value)
	case "TXT":
		if fromWire && strings.HasPrefix(value, `"`) {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return "", invalid("invalid quoted TXT value")
			}
			value = decoded
		}
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
			return "", invalid("invalid TXT value")
		}
		return value, nil
	default:
		return "", invalid("unsupported record type")
	}
}

func owner(value string) string {
	labels := make(map[string]string)
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || labels[k] != "" {
			return ""
		}
		labels[k] = v
	}
	if labels["heritage"] != "external-dns" {
		return ""
	}
	return labels["external-dns/owner"]
}

func markerBase(name string) (string, string, bool) {
	for _, typ := range []string{"A", "AAAA", "CNAME"} {
		prefix := "edns-" + strings.ToLower(typ) + "."
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix), typ, true
		}
	}
	return "", "", false
}

func (p *Provider) normalize(ep Endpoint, desired bool) (Endpoint, error) {
	var err error
	ep.DNSName, err = domain(ep.DNSName)
	if err != nil {
		return ep, err
	}
	if !p.allowed(ep.DNSName) {
		return ep, &Error{Status: 403, Message: "record is outside configured domain filters"}
	}
	ep.RecordType = strings.ToUpper(ep.RecordType)
	if !supported(ep.RecordType) {
		return ep, invalid("supported types are A, AAAA, CNAME, and ownership TXT")
	}
	if ep.SetIdentifier != "" || len(ep.ProviderSpecific) != 0 {
		return ep, invalid("routing policies and provider-specific options are not supported")
	}
	if ep.RecordTTL == 0 {
		ep.RecordTTL = int64(p.ttl)
	}
	if desired && (ep.RecordTTL < 300 || ep.RecordTTL > 86400) {
		return ep, invalid("TTL must be between 300 and 86400 seconds")
	}
	if len(ep.Targets) == 0 {
		return ep, invalid("record must have a target")
	}
	values := make([]string, 0, len(ep.Targets))
	for _, value := range ep.Targets {
		value, err = target(ep.RecordType, value, true)
		if err != nil {
			return ep, err
		}
		if ep.RecordType == "TXT" {
			base, _, ok := markerBase(ep.DNSName)
			if !ok || !p.allowed(base) || owner(value) != p.owner {
				return ep, invalid("TXT writes must contain this controller's plain-text ownership metadata using edns-%{record_type}. prefix")
			}
		}
		values = append(values, value)
	}
	slices.Sort(values)
	ep.Targets = slices.Compact(values)
	if ep.RecordType == "CNAME" && len(ep.Targets) != 1 {
		return ep, invalid("CNAME requires exactly one target")
	}
	if ep.RecordType == "TXT" && len(ep.Targets) != 1 {
		return ep, invalid("ownership TXT requires exactly one value")
	}
	return ep, nil
}

func wire(ep Endpoint) Endpoint {
	if ep.RecordType == "TXT" {
		ep.Targets = slices.Clone(ep.Targets)
		for i, value := range ep.Targets {
			ep.Targets[i] = strconv.Quote(value)
		}
	}
	return ep
}

func (p *Provider) Adjust(endpoints []Endpoint) ([]Endpoint, error) {
	result := make([]Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		normalized, err := p.normalize(ep, true)
		if err != nil {
			return nil, err
		}
		result = append(result, wire(normalized))
	}
	return result, nil
}
