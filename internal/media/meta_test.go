package media

import (
	"testing"
	"time"
)

func TestTimeFromFilename(t *testing.T) {
	cases := map[string]string{
		"IMG_20240131_142530.jpg":               "2024-01-31T14:25:30",
		"PXL_20230717_143721529.jpg":            "2023-07-17T14:37:21",
		"VID_20240131_142530.mp4":               "2024-01-31T14:25:30",
		"Screenshot_20240131-142530.png":        "2024-01-31T14:25:30",
		"signal-2024-01-31-142530.jpg":          "2024-01-31T14:25:30",
		"Screenshot 2024-01-31 at 14.25.30.png": "2024-01-31T14:25:30",
		"20230610_140418_e50e.mp4":              "2023-06-10T14:04:18",
		"holiday.jpg":                           "",
		"IMG_20241345_142530.jpg":               "", // month 13
		"DSC01234.JPG":                          "",
	}
	for name, want := range cases {
		got, ok := TimeFromFilename(name)
		s := ""
		if ok {
			s = got.Format("2006-01-02T15:04:05")
		}
		if s != want {
			t.Errorf("%s: got %q, want %q", name, s, want)
		}
	}
}

func TestOffsetFrom(t *testing.T) {
	utc := time.Date(2023, 6, 10, 12, 4, 35, 0, time.UTC)
	local := time.Date(2023, 6, 10, 14, 4, 18, 0, time.UTC) // wall clock, start of a 32 s clip
	if o := offsetFrom(local, utc, 32777); o == nil || *o != 120 {
		t.Errorf("CEST clip: %v", o)
	}
	// India, +05:30.
	if o := offsetFrom(utc.Add(5*time.Hour+30*time.Minute), utc, 0); o == nil || *o != 330 {
		t.Errorf("IST: %v", o)
	}
	// Name and timestamp disagree by more than slack: no guess.
	if o := offsetFrom(utc.Add(2*time.Hour+7*time.Minute), utc, 1000); o != nil {
		t.Errorf("unrelated times gave %d", *o)
	}
	if o := offsetFrom(utc.Add(20*time.Hour), utc, 0); o != nil {
		t.Errorf("20h apart gave %d", *o)
	}
}
