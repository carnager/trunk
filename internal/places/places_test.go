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

func TestSearch(t *testing.T) {
	first := func(q string) string {
		r := Search(q, 5)
		if len(r) == 0 {
			return ""
		}
		n := r[0].In(language.German)
		return n.City + ", " + n.CC
	}
	for q, want := range map[string]string{
		"berlin":   "Berlin, DE", // the capital, not Berlín in Honduras
		"Wien":     "Wien, AT",
		"vienna":   "Wien, AT", // an English name finds it too
		"münchen":  "München, DE",
		"paris":    "Paris, FR",
		"schwelm":  "Schwelm, DE",
		"lichtens": "", // filled below: a prefix finds something
	} {
		if want == "" {
			continue
		}
		if got := first(q); got != want {
			t.Errorf("%q: %q, want %q", q, got, want)
		}
	}
	if len(Search("lichtens", 5)) == 0 {
		t.Error("prefix finds nothing")
	}
	// Other spellings rank after real names: Austria's Wien… towns before
	// Thailand's Wieng Tai.
	for _, p := range Search("wien", 6) {
		if p.Country == "TH" {
			t.Error("a town found by another spelling ranks among the first")
		}
	}
	if len(Search("", 5)) != 0 || len(Search("zzqqxx", 5)) != 0 {
		t.Error("nonsense finds something")
	}
}
