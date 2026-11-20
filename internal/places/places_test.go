package places

import (
	"testing"

	"golang.org/x/text/language"
)

func TestNearest(t *testing.T) {
	de := language.German
	cases := []struct {
		lat, lon     float64
		city, region string
		country      string
	}{
		{48.2085, 16.3721, "Wien", "-", "Österreich"}, // Stephansplatz; "-": no region, the city is one
		{48.1374, 11.5755, "München", "Bayern", "Deutschland"},
		{41.9029, 12.4534, "Vatikanstadt", "", "Vatikanstadt"},
		{52.5163, 13.3777, "Berlin", "-", "Deutschland"}, // Brandenburger Tor, not "Tiergarten"
	}
	for _, c := range cases {
		p := Nearest(c.lat, c.lon)
		if p == nil {
			t.Errorf("%v,%v: no place", c.lat, c.lon)
			continue
		}
		n := p.In(de)
		want := c.region
		if want == "-" {
			want = ""
		}
		if n.City != c.city || n.Country != c.country || (c.region != "" && n.Region != want) {
			t.Errorf("%v,%v: %+v, want %s, %s", c.lat, c.lon, n, c.city, c.country)
		}
	}
	// English, and the fallback for languages without names of their own.
	if n := Nearest(48.2085, 16.3721).In(language.English); n.City != "Vienna" || n.Country != "Austria" {
		t.Errorf("english: %+v", n)
	}
	if n := Nearest(48.2085, 16.3721).In(language.Japanese); n.City != "Vienna" {
		t.Errorf("fallback: %+v", n)
	}
	// Middle of the Atlantic: nothing within reach.
	if p := Nearest(35, -40); p != nil {
		t.Errorf("atlantic: %+v", p.In(de))
	}
}

func TestMatches(t *testing.T) {
	vienna := Nearest(48.2085, 16.3721)
	for _, q := range []string{"wien", "vienna", "viena", "österreich", "austria"} {
		if !vienna.Matches(q) {
			t.Errorf("Vienna should match %q", q)
		}
	}
	if vienna.Matches("münchen") {
		t.Error("Vienna matches münchen")
	}
}
