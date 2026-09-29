package siding

import (
	"errors"
	"net/url"
	"strings"
)

// W3C Baggage limits (https://www.w3.org/TR/baggage/#limits). A header over
// maxBaggageBytes is ignored outright rather than partially parsed, so a
// client can't push routing keys past the point a downstream parser stops.
const (
	maxBaggageBytes   = 8192
	maxBaggageMembers = 180
)

var errBaggageTooLarge = errors.New("baggage exceeds W3C limits")

// member is one baggage list-member. A member that fails to parse keeps its
// raw text so re-serializing the header passes it through untouched instead
// of silently dropping another system's data. One with a readable key but an
// undecodable value is malformed: it has no value to read, but del and set
// still find it by key, so a routing key can't be carried past a strip by
// breaking its percent-encoding.
type member struct {
	key       string
	value     string // percent-decoded
	props     string // raw ";"-separated properties, without the leading ";"
	raw       string
	malformed bool
}

// baggage is an ordered W3C baggage list.
type baggage struct {
	members []member
	changed bool
}

// parseBaggage parses every value of the baggage header (multiple header
// lines are one comma-joined list per RFC 9110).
func parseBaggage(values []string) (*baggage, error) {
	b := &baggage{}
	joined := strings.Join(values, ",")
	if len(joined) > maxBaggageBytes {
		return b, errBaggageTooLarge
	}
	for _, part := range strings.Split(joined, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if len(b.members) == maxBaggageMembers {
			return &baggage{}, errBaggageTooLarge
		}
		b.members = append(b.members, parseMember(part))
	}
	return b, nil
}

func parseMember(part string) member {
	raw := strings.TrimSpace(part)
	kv, props, _ := strings.Cut(raw, ";")
	key, value, ok := strings.Cut(kv, "=")
	key = strings.TrimSpace(key)
	if !ok || !isToken(key) {
		return member{raw: raw}
	}
	decoded, err := url.PathUnescape(strings.TrimSpace(value))
	if err != nil {
		return member{key: key, raw: raw, malformed: true}
	}
	return member{key: key, value: decoded, props: strings.TrimSpace(props), raw: raw}
}

// clone copies the list into an array of its own: del, delFold and keepFirst
// filter the one they are given in place.
func (b *baggage) clone() *baggage {
	members := make([]member, len(b.members), len(b.members)+2)
	copy(members, b.members)
	return &baggage{members: members, changed: b.changed}
}

// fits reports whether the list is within the W3C limits, and so whether
// the next hop will read it.
func (b *baggage) fits() bool {
	return len(b.members) <= maxBaggageMembers && len(b.String()) <= maxBaggageBytes
}

// get returns the first well-formed member with key.
func (b *baggage) get(key string) (string, bool) {
	for _, m := range b.members {
		if m.key == key && !m.malformed {
			return m.value, true
		}
	}
	return "", false
}

// keepFirst leaves at most one member with key: the first well-formed one.
// Duplicates, malformed members and members whose key differs from key only
// in case are removed, so a later hop whose parser prefers the last member,
// tolerates a bad escape or folds case reads the same value this one acted
// on.
func (b *baggage) keepFirst(key string) {
	out := b.members[:0]
	kept := false
	for _, m := range b.members {
		if strings.EqualFold(m.key, key) {
			if kept || m.malformed || m.key != key {
				b.changed = true
				continue
			}
			kept = true
		}
		out = append(out, m)
	}
	b.members = out
}

// set replaces every member with key by a single one holding value, at the
// position of the first, or appends it.
func (b *baggage) set(key, value string) {
	out := make([]member, 0, len(b.members)+1)
	placed := false
	for _, m := range b.members {
		if m.key != key {
			out = append(out, m)
			continue
		}
		if !placed {
			out = append(out, member{key: key, value: value})
			placed = true
		}
	}
	if !placed {
		out = append(out, member{key: key, value: value})
	}
	b.members = out
	b.changed = true
}

// del removes every member with key.
func (b *baggage) del(key string) {
	out := b.members[:0]
	for _, m := range b.members {
		if m.key == key {
			b.changed = true
			continue
		}
		out = append(out, m)
	}
	b.members = out
}

// delFold is del for a routing key: it removes the key in any case. Baggage
// keys are case-sensitive, but a strip has to hold against a later parser
// that is not.
func (b *baggage) delFold(key string) {
	out := b.members[:0]
	for _, m := range b.members {
		if strings.EqualFold(m.key, key) {
			b.changed = true
			continue
		}
		out = append(out, m)
	}
	b.members = out
}

// String serializes the list; untouched members keep their original text.
func (b *baggage) String() string {
	parts := make([]string, 0, len(b.members))
	for _, m := range b.members {
		if m.raw != "" {
			parts = append(parts, m.raw)
			continue
		}
		s := m.key + "=" + encodeBaggageValue(m.value)
		if m.props != "" {
			s += ";" + m.props
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

// encodeBaggageValue percent-encodes everything outside W3C baggage-octet
// (%x21 / %x23-2B / %x2D-3A / %x3C-5B / %x5D-7E), plus "%" itself so the
// value decodes back unchanged.
func encodeBaggageValue(v string) string {
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if isBaggageOctet(c) && c != '%' {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte(hex[c>>4])
		sb.WriteByte(hex[c&0x0f])
	}
	return sb.String()
}

func isBaggageOctet(c byte) bool {
	return c == 0x21 ||
		(c >= 0x23 && c <= 0x2B) ||
		(c >= 0x2D && c <= 0x3A) ||
		(c >= 0x3C && c <= 0x5B) ||
		(c >= 0x5D && c <= 0x7E)
}

// isToken reports whether s is an RFC 9110 token, the W3C baggage key syntax.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	// One boolean expression rather than a multi-expression switch case:
	// Yaegi v0.16.1 only tests the first expression of a tagless case list.
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && strings.IndexByte("!#$%&'*+-.^_`|~", c) < 0 {
			return false
		}
	}
	return true
}
