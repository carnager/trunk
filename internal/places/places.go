// Package places turns coordinates into place names, offline: the nearest
// town with at least 1000 inhabitants from GeoNames (CC BY 4.0,
// https://www.geonames.org), built into the binary. Regenerate the data with
// tools/placesgen.
package places

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

//go:embed places.tsv.gz
var data []byte

// Place is a town with its region and country.
type Place struct {
	ID      int64 // GeoNames id; what trunk stores per asset
	Lat     float64
	Lon     float64
	Country string // ISO 3166 code, e.g. AT
	Pop     int    // population, for ranking search results
	region  string // GeoNames admin1 key, e.g. AT.09
	names   []string
	aka     []string // other names, for search
}

// Names are in the order of the languages in the data file: the main name,
// then one per language (empty where it is the same as the main name).
var langs []string

var (
	once    sync.Once
	byID    map[int64]*Place
	regions map[string][]string
	grid    map[[2]int][]*Place
)

func load() {
	once.Do(func() {
		byID = map[int64]*Place{}
		regions = map[string][]string{}
		grid = map[[2]int][]*Place{}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			panic("places: " + err.Error())
		}
		sc := bufio.NewScanner(zr)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "#") {
				if i := strings.Index(line, "languages: "); i >= 0 {
					langs = strings.Split(line[i+len("languages: "):], ",")
				}
				continue
			}
			f := strings.Split(line, "\t")
			switch f[0] {
			case "R":
				regions[f[1]] = f[2:]
			case "C":
				id, _ := strconv.ParseInt(f[1], 10, 64)
				lat, _ := strconv.ParseFloat(f[2], 64)
				lon, _ := strconv.ParseFloat(f[3], 64)
				n := len(langs) + 1
				p := &Place{ID: id, Lat: lat, Lon: lon, Country: f[4], region: f[4] + "." + f[5], names: f[6 : 6+n]}
				if alts := f[6+n]; alts != "" {
					p.aka = strings.Split(alts, "|")
				}
				if len(f) > 7+n {
					p.Pop, _ = strconv.Atoi(f[7+n])
				}
				byID[id] = p
				k := cell(lat, lon)
				grid[k] = append(grid[k], p)
			}
		}
	})
}

func cell(lat, lon float64) [2]int {
	return [2]int{int(math.Floor(lat)), int(math.Floor(lon))}
}

// MaxDistance is how far the nearest town may be for a photo to be placed in it.
const MaxDistance = 30.0 // km

// Nearest finds the town closest to a position, or nil if none is within MaxDistance.
func Nearest(lat, lon float64) *Place {
	load()
	// One degree of latitude is 111 km; of longitude, less towards the poles.
	dLon := int(math.Ceil(MaxDistance / (111 * math.Max(math.Cos(lat*math.Pi/180), 0.05))))
	c := cell(lat, lon)
	var best *Place
	bestD := MaxDistance
	for y := c[0] - 1; y <= c[0]+1; y++ {
		for x := c[1] - dLon; x <= c[1]+dLon; x++ {
			xx := x
			if xx < -180 {
				xx += 360
			} else if xx >= 180 {
				xx -= 360
			}
			for _, p := range grid[[2]int{y, xx}] {
				if d := distance(lat, lon, p.Lat, p.Lon); d < bestD {
					best, bestD = p, d
				}
			}
		}
	}
	return best
}

// ByID returns a place by its GeoNames id.
func ByID(id int64) *Place {
	load()
	return byID[id]
}

func distance(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	φ1, φ2 := lat1*math.Pi/180, lat2*math.Pi/180
	dφ, dλ := φ2-φ1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dφ/2)*math.Sin(dφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// Names is a place spelled out in one language.
type Names struct {
	City    string `json:"city"`
	Region  string `json:"region,omitempty"`
	Country string `json:"country,omitempty"`
	CC      string `json:"cc"`
}

// In returns the place's names in the best of the given languages (as from an
// Accept-Language header), falling back to GeoNames' main names.
func (p *Place) In(tags ...language.Tag) Names {
	load()
	idx := 0 // main name
	base := "en"
	for _, t := range tags {
		b, _ := t.Base()
		for i, l := range langs {
			if b.String() == l {
				idx, base = i+1, l
				break
			}
		}
		if idx > 0 {
			break
		}
	}
	pick := func(names []string) string {
		if idx > 0 && idx < len(names) && names[idx] != "" {
			return names[idx]
		}
		if len(names) > 0 {
			return names[0]
		}
		return ""
	}
	n := Names{City: pick(p.names), Region: pick(regions[p.region]), CC: p.Country}
	// City states: "Wien, Bundesland Wien" says the same thing twice.
	if strings.Contains(n.Region, n.City) {
		n.Region = ""
	}
	if r, err := language.ParseRegion(p.Country); err == nil {
		n.Country = display.Regions(language.Make(base)).Name(r)
	}
	return n
}

// Matches reports whether query (lower case) is part of any of the place's
// names, in any language, including region and country.
func (p *Place) Matches(query string) bool {
	load()
	for _, set := range [][]string{p.names, p.aka, regions[p.region]} {
		for _, n := range set {
			if n != "" && strings.Contains(strings.ToLower(n), query) {
				return true
			}
		}
	}
	if r, err := language.ParseRegion(p.Country); err == nil {
		for _, l := range append([]string{"en"}, langs...) {
			if strings.Contains(strings.ToLower(display.Regions(language.Make(l)).Name(r)), query) {
				return true
			}
		}
	}
	return false
}

// Search finds towns by name, in any language: names that match exactly
// first, then names that start with the query, then names that contain it;
// among equals, bigger towns first. query is matched case-insensitively.
func Search(query string, limit int) []*Place {
	load()
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	type hit struct {
		p    *Place
		rank int // 0 exact, 1 prefix, 2 contained
	}
	var hits []hit
	for _, p := range byID {
		best := 3
		for _, set := range [][]string{p.names, p.aka} {
			for _, n := range set {
				if n == "" {
					continue
				}
				l := strings.ToLower(n)
				switch {
				case l == q:
					best = 0
				case best > 1 && strings.HasPrefix(l, q):
					best = 1
				case best > 2 && strings.Contains(l, q):
					best = 2
				}
			}
		}
		if best < 3 {
			hits = append(hits, hit{p, best})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].p.Pop > hits[j].p.Pop
	})
	out := make([]*Place, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.p)
	}
	return out
}
