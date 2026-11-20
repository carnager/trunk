// Command placesgen builds internal/places/places.tsv.gz from a GeoNames dump.
//
// Download into one folder from https://download.geonames.org/export/dump/:
// cities1000.zip (unzipped: cities1000.txt), admin1CodesASCII.txt and
// alternateNamesV2.zip (unzipped: alternateNamesV2.txt), then run
//
//	go run ./tools/placesgen -src DIR
//
// GeoNames data is CC BY 4.0 (https://www.geonames.org).
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Languages whose names are kept for display, besides GeoNames' main name.
var langs = []string{"de"}

func main() {
	src := flag.String("src", ".", "folder with the GeoNames files")
	out := flag.String("out", "internal/places/places.tsv.gz", "output file")
	flag.Parse()

	type city struct {
		id, name, lat, lon, cc, admin1 string
		alts                           []string
	}
	var cities []city
	want := map[string]bool{}
	each(filepath.Join(*src, "cities1000.txt"), func(f []string) {
		if len(f) < 15 {
			return
		}
		// Not parts of a city (Berlin's centre is Berlin, not Tiergarten), nor places
		// that are abandoned, destroyed or only historical.
		switch f[7] {
		case "PPLX", "PPLH", "PPLQ", "PPLW", "PPLCH":
			return
		}
		c := city{id: f[0], name: f[1], lat: trim(f[4]), lon: trim(f[5]), cc: f[8], admin1: f[10]}
		seen := map[string]bool{strings.ToLower(f[1]): true}
		for _, a := range append([]string{f[2]}, strings.Split(f[3], ",")...) {
			if a == "" || len(a) > 40 || !latin(a) || seen[strings.ToLower(a)] {
				continue
			}
			seen[strings.ToLower(a)] = true
			c.alts = append(c.alts, a)
		}
		cities = append(cities, c)
		want[c.id] = true
	})

	type region struct{ key, name, id string }
	var regions []region
	each(filepath.Join(*src, "admin1CodesASCII.txt"), func(f []string) {
		if len(f) >= 4 {
			regions = append(regions, region{f[0], f[1], f[3]})
			want[f[3]] = true
		}
	})

	// Names per language. Short and preferred forms win: "Bayern" over
	// "Freistaat Bayern", "Wien" over "Bundesland Wien".
	type local struct {
		name  string
		score int
	}
	names := map[string]map[string]local{} // lang → geoname id → name
	for _, l := range langs {
		names[l] = map[string]local{}
	}
	each(filepath.Join(*src, "alternateNamesV2.txt"), func(f []string) {
		if len(f) < 8 || !want[f[1]] {
			return
		}
		m, ok := names[f[2]]
		if !ok {
			return
		}
		// Skip colloquial and historic names, and names valid only for a time.
		if f[6] == "1" || f[7] == "1" || (len(f) > 9 && (f[8] != "" || f[9] != "")) {
			return
		}
		score := 1
		if f[4] == "1" {
			score += 2
		}
		if f[5] == "1" {
			score++
		}
		if cur, ok := m[f[1]]; !ok || score > cur.score {
			m[f[1]] = local{f[3], score}
		}
	})

	file, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(file, gzip.BestCompression)
	w := bufio.NewWriter(zw)
	fmt.Fprintf(w, "# GeoNames (CC BY 4.0, https://www.geonames.org); languages: %s\n", strings.Join(langs, ","))
	// Regions: R key name name_<lang>...
	for _, r := range regions {
		fmt.Fprintf(w, "R\t%s\t%s", r.key, r.name)
		for _, l := range langs {
			fmt.Fprintf(w, "\t%s", localName(names[l][r.id].name, r.name))
		}
		fmt.Fprintln(w)
	}
	// Cities: C id lat lon cc admin1 name name_<lang>... alts(|)
	for _, c := range cities {
		fmt.Fprintf(w, "C\t%s\t%s\t%s\t%s\t%s\t%s", c.id, c.lat, c.lon, c.cc, c.admin1, c.name)
		for _, l := range langs {
			fmt.Fprintf(w, "\t%s", localName(names[l][c.id].name, c.name))
		}
		fmt.Fprintf(w, "\t%s\n", strings.Join(c.alts, "|"))
	}
	if err := w.Flush(); err != nil {
		log.Fatal(err)
	}
	zw.Close()
	file.Close()
	st, _ := os.Stat(*out)
	log.Printf("%d cities, %d regions, %s: %d KB", len(cities), len(regions), *out, st.Size()/1024)
}

// localName is empty when it adds nothing over the main name.
func localName(n, main string) string {
	if n == main {
		return ""
	}
	return n
}

func trim(coord string) string {
	// Four decimals are about 10 m: plenty to find the nearest town.
	if i := strings.IndexByte(coord, '.'); i >= 0 && len(coord) > i+5 {
		return coord[:i+5]
	}
	return coord
}

func latin(s string) bool {
	for _, r := range s {
		if r > 0x24F && !(r >= 0x1E00 && r <= 0x1EFF) && !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			return false
		}
	}
	return true
}

func each(path string, fn func([]string)) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		fn(strings.Split(sc.Text(), "\t"))
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
}
