package server

import (
	"fmt"
	"net/http"
	"time"

	"golang.org/x/text/language"
)

// The public page speaks the visitor's language: English, or German for
// browsers that prefer it. Place names follow the same choice.

type shareLang struct {
	Tag                           language.Tag
	Code                          string
	GoneTitle, GoneHead, GoneText string
	SharedBy, SharedWith          string
	Open, Photo                   string
	Close, Prev, Next, Download   string
	DefaultTitle                  string
	photos, videos                func(n int) string
	and                           string
	months                        []string
	day                           func(t time.Time, months []string) string
	days                          func(lo, hi time.Time, months []string) string
	monthsSpan                    func(lo, hi time.Time, months []string) string
}

var shareEN = &shareLang{
	Tag: language.English, Code: "en",
	GoneTitle: "Link expired", GoneHead: "This link no longer works",
	GoneText: "It has expired or was withdrawn. Best ask the person who sent it to you.",
	SharedBy: "shared by", SharedWith: "Shared with", Open: "Open", Photo: "Photo",
	Close: "Close", Prev: "Previous", Next: "Next", Download: "Download original",
	DefaultTitle: "Photos",
	photos:       func(n int) string { return plural(n, "photo", "photos") },
	videos:       func(n int) string { return plural(n, "video", "videos") },
	and:          "and",
	months:       []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	day: func(t time.Time, m []string) string {
		return fmt.Sprintf("%s %d, %d", m[t.Month()-1], t.Day(), t.Year())
	},
	days: func(lo, hi time.Time, m []string) string {
		return fmt.Sprintf("%s %d–%d, %d", m[hi.Month()-1], lo.Day(), hi.Day(), hi.Year())
	},
	monthsSpan: func(lo, hi time.Time, m []string) string {
		return fmt.Sprintf("%s – %s %d", m[lo.Month()-1], m[hi.Month()-1], hi.Year())
	},
}

var shareDE = &shareLang{
	Tag: language.German, Code: "de",
	GoneTitle: "Link abgelaufen", GoneHead: "Dieser Link gilt nicht mehr",
	GoneText: "Er ist abgelaufen oder wurde zurückgezogen. Frag am besten die Person, die ihn dir geschickt hat.",
	SharedBy: "geteilt von", SharedWith: "Geteilt mit", Open: "öffnen", Photo: "Bild",
	Close: "Schließen", Prev: "Zurück", Next: "Weiter", Download: "Original laden",
	DefaultTitle: "Fotos",
	photos:       func(n int) string { return plural(n, "Foto", "Fotos") },
	videos:       func(n int) string { return plural(n, "Video", "Videos") },
	and:          "und",
	months:       []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"},
	day: func(t time.Time, m []string) string {
		return fmt.Sprintf("%d. %s %d", t.Day(), m[t.Month()-1], t.Year())
	},
	days: func(lo, hi time.Time, m []string) string {
		return fmt.Sprintf("%d.–%d. %s %d", lo.Day(), hi.Day(), m[hi.Month()-1], hi.Year())
	},
	monthsSpan: func(lo, hi time.Time, m []string) string {
		return fmt.Sprintf("%s – %s %d", m[lo.Month()-1], m[hi.Month()-1], hi.Year())
	},
}

var shareMatcher = language.NewMatcher([]language.Tag{language.English, language.German})

// shareLanguage picks the page's language from the browser's preferences.
func shareLanguage(r *http.Request) *shareLang {
	tags, _, _ := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	if _, i, conf := shareMatcher.Match(tags...); i == 1 && conf != language.No {
		return shareDE
	}
	return shareEN
}

func plural(n int, one, other string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, other)
}

// Date says when a photo was taken.
func (l *shareLang) Date(a *Asset) string {
	t, ok := localDate(a)
	if !ok {
		return ""
	}
	return l.day(t, l.months)
}

// dateRange says when the photos were taken, as briefly as it can:
// "July 17, 2023", "July 17–20, 2023", "July – September 2023", "2019 – 2023".
func (l *shareLang) dateRange(list []*Asset) string {
	var lo, hi time.Time
	for _, a := range list {
		if t, ok := localDate(a); ok {
			if lo.IsZero() || t.Before(lo) {
				lo = t
			}
			if t.After(hi) {
				hi = t
			}
		}
	}
	switch {
	case lo.IsZero():
		return ""
	case lo.Year() != hi.Year():
		return fmt.Sprintf("%d – %d", lo.Year(), hi.Year())
	case lo.Month() != hi.Month():
		return l.monthsSpan(lo, hi, l.months)
	case lo.Day() != hi.Day():
		return l.days(lo, hi, l.months)
	}
	return l.day(lo, l.months)
}

// count says how many photos and videos there are: "3 photos and 1 video".
func (l *shareLang) count(photos, videos int) string {
	switch {
	case videos == 0:
		return l.photos(photos)
	case photos == 0:
		return l.videos(videos)
	}
	return l.photos(photos) + " " + l.and + " " + l.videos(videos)
}
