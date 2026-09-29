package siding

import (
	"strings"
	"testing"
)

func TestParseBaggage(t *testing.T) {
	b, err := parseBaggage([]string{
		"request-tenancy=test%2Fenv-1;ttl=60 , userId=alice",
		"other=a%20b%2Cc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"request-tenancy": "test/env-1",
		"userId":          "alice",
		"other":           "a b,c",
	} {
		if got, ok := b.get(key); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", key, got, ok, want)
		}
	}
	if b.members[0].props != "ttl=60" {
		t.Errorf("props = %q", b.members[0].props)
	}
	if _, ok := b.get("missing"); ok {
		t.Error("found missing key")
	}
}

func TestParseBaggageFirstDuplicateWins(t *testing.T) {
	b, _ := parseBaggage([]string{"k=1,k=2"})
	if v, _ := b.get("k"); v != "1" {
		t.Fatalf("got %q", v)
	}
}

func TestParseBaggageKeepsMalformedMembers(t *testing.T) {
	in := "noequals, bad key=x ,pct=%zz,ok=1"
	b, err := parseBaggage([]string{in})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := b.get("ok"); !ok || v != "1" {
		t.Fatalf("ok = %q", v)
	}
	if _, ok := b.get("pct"); ok {
		t.Error("member with an invalid escape was parsed")
	}
	b.set("new", "v")
	want := "noequals,bad key=x,pct=%zz,ok=1,new=v"
	if got := b.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseBaggageLimits(t *testing.T) {
	if _, err := parseBaggage([]string{strings.Repeat("a", maxBaggageBytes+1)}); err != errBaggageTooLarge {
		t.Errorf("oversized header: %v", err)
	}
	many := strings.Repeat("k=v,", maxBaggageMembers+1)
	if _, err := parseBaggage([]string{many}); err != errBaggageTooLarge {
		t.Errorf("too many members: %v", err)
	}
	b, err := parseBaggage(nil)
	if err != nil || len(b.members) != 0 {
		t.Errorf("empty: %+v, %v", b, err)
	}
}

func TestBaggageSetAndDel(t *testing.T) {
	b, _ := parseBaggage([]string{"a=1,k=old;p,b=2,k=dup"})
	if b.changed {
		t.Fatal("changed before any edit")
	}
	b.set("k", "new value/%")
	if got, want := b.String(), "a=1,k=new%20value/%25,b=2"; got != want {
		t.Fatalf("set: got %q, want %q", got, want)
	}
	b.del("a")
	b.set("z", "last")
	if got, want := b.String(), "k=new%20value/%25,b=2,z=last"; got != want {
		t.Fatalf("del+append: got %q, want %q", got, want)
	}
	if !b.changed {
		t.Fatal("changed not set")
	}

	// Values round-trip through encode/parse.
	back, _ := parseBaggage([]string{b.String()})
	if v, _ := back.get("k"); v != "new value/%" {
		t.Fatalf("round trip: %q", v)
	}
}

func TestBaggageDelMissingIsNoChange(t *testing.T) {
	b, _ := parseBaggage([]string{"a=1"})
	b.del("nope")
	if b.changed {
		t.Fatal("deleting a missing key marked the baggage changed")
	}
}

// A member with a readable key and a value that doesn't decode has nothing
// to read, but it can still be found to be removed.
func TestBaggageMalformedMemberIsFoundByKey(t *testing.T) {
	b, _ := parseBaggage([]string{"a=1,k=%zz,b=2"})
	if _, ok := b.get("k"); ok {
		t.Fatal("malformed member has a value")
	}
	b.del("k")
	if got, want := b.String(), "a=1,b=2"; got != want || !b.changed {
		t.Fatalf("del: got %q, changed %v", got, b.changed)
	}

	b, _ = parseBaggage([]string{"a=1,k=%zz,b=2"})
	b.set("k", "v")
	if got, want := b.String(), "a=1,k=v,b=2"; got != want {
		t.Fatalf("set: got %q, want %q", got, want)
	}
}

func TestBaggageKeepFirst(t *testing.T) {
	for in, want := range map[string]string{
		"a=1,k=first;p,b=2,k=second": "a=1,k=first;p,b=2",
		"k=%zz,k=good,k=later":       "k=good",
		"k=%zz,a=1":                  "a=1",
	} {
		b, _ := parseBaggage([]string{in})
		b.keepFirst("k")
		if got := b.String(); got != want || !b.changed {
			t.Errorf("%q: got %q, changed %v; want %q", in, got, b.changed, want)
		}
	}
	// One well-formed member is left exactly as it came.
	b, _ := parseBaggage([]string{"a=1, k=only;p ,kk=other"})
	b.keepFirst("k")
	if b.changed {
		t.Fatal("a single member marked the baggage changed")
	}
	// A key that differs only in case goes, wherever it stands.
	b, _ = parseBaggage([]string{"K=first,a=1,k=exact,K=last"})
	b.keepFirst("k")
	if got, want := b.String(), "a=1,k=exact"; got != want || !b.changed {
		t.Fatalf("got %q, changed %v; want %q", got, b.changed, want)
	}
}

func TestBaggageDelFold(t *testing.T) {
	b, _ := parseBaggage([]string{"a=1,Request-Tenancy=x,request-tenancy=%zz,REQUEST-TENANCY=y,b=2"})
	b.delFold("request-tenancy")
	if got, want := b.String(), "a=1,b=2"; got != want || !b.changed {
		t.Fatalf("got %q, changed %v", got, b.changed)
	}
	b, _ = parseBaggage([]string{"a=1"})
	b.delFold("request-tenancy")
	if b.changed {
		t.Fatal("nothing to delete marked the baggage changed")
	}
}

func TestBaggageCloneIsIndependent(t *testing.T) {
	b, _ := parseBaggage([]string{"a=1,k=2,b=3"})
	c := b.clone()
	c.del("a")
	c.set("z", "9")
	if got, want := b.String(), "a=1,k=2,b=3"; got != want || b.changed {
		t.Fatalf("the original changed: %q, changed %v", got, b.changed)
	}
	if got, want := c.String(), "k=2,b=3,z=9"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBaggageFits(t *testing.T) {
	for in, want := range map[string]bool{
		"": true,
		strings.TrimSuffix(strings.Repeat("k=v,", maxBaggageMembers), ","):   true,
		strings.TrimSuffix(strings.Repeat("k=v,", maxBaggageMembers+1), ","): false,
		"k=" + strings.Repeat("x", maxBaggageBytes-2):                        true,
		"k=" + strings.Repeat("x", maxBaggageBytes-1):                        false,
	} {
		b := &baggage{}
		for _, part := range strings.Split(in, ",") {
			if part != "" {
				b.members = append(b.members, parseMember(part))
			}
		}
		if got := b.fits(); got != want {
			t.Errorf("%d members, %d bytes: fits = %v", len(b.members), len(in), got)
		}
	}
}
