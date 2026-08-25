package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oppai/puffy/internal/model"
)

func sample(t float64, rtt *float64) model.Sample {
	return model.Sample{Round: int(t / 1000), OffsetMS: t, RTTMS: rtt, Addr: "198.51.100.1"}
}

func ptr(v float64) *float64 { return &v }

func testSession() *model.Session {
	s := &model.Session{
		Tool: "puffy", Version: "test", Schema: 1, Mode: "trace",
		Target: "example.test", TargetIP: "198.51.100.9", TargetName: "example.test",
		StartedAt:  time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		EndedAt:    time.Date(2026, 8, 25, 12, 0, 10, 0, time.UTC),
		IntervalMS: 1000, TimeoutMS: 2000, Rounds: 10,
	}
	for ttl := 1; ttl <= 3; ttl++ {
		h := &model.Hop{TTL: ttl, Addrs: []string{"198.51.100.1"}, Final: ttl == 3}
		for r := range 10 {
			var rtt *float64
			if !(ttl == 2 && r >= 4 && r < 7) { // a loss burst at the middle hop
				rtt = ptr(float64(ttl) * 10)
			}
			h.Samples = append(h.Samples, sample(float64(r)*1000, rtt))
		}
		s.Hops = append(s.Hops, h)
	}
	s.Analyze()
	return s
}

func TestJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")
	want := testSession()

	if err := WriteJSON(want, path, true); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	got, err := ReadJSON(path)
	if err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}

	if got.Mode != want.Mode || got.TargetIP != want.TargetIP || got.Rounds != want.Rounds {
		t.Errorf("header changed: %+v", got)
	}
	if len(got.Hops) != len(want.Hops) {
		t.Fatalf("hops = %d, want %d", len(got.Hops), len(want.Hops))
	}
	// A lost probe must survive as null rather than becoming a zero, which
	// would silently turn 30% loss into 30% of instant replies.
	var lost int
	for _, s := range got.Hops[1].Samples {
		if s.Lost() {
			lost++
		}
	}
	if lost != 3 {
		t.Errorf("lost samples survived as %d, want 3", lost)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, want.StartedAt)
	}
}

// The parent directory is created for the caller: nobody wants a run discarded
// because reports/ did not exist yet.
func TestWriteJSONCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "run.json")
	if err := WriteJSON(testSession(), path, false); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestReadJSONRejectsForeignDocuments(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"not puffy":     `{"tool":"mtr","hops":[{"ttl":1}]}`,
		"future schema": `{"tool":"puffy","schema":99,"hops":[{"ttl":1}]}`,
		"no hops":       `{"tool":"puffy","schema":1,"hops":[]}`,
		"not json":      `this is not json`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadJSON(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWriteHTMLIsSelfContained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.html")
	if err := WriteHTML(testSession(), path, "auto"); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	// Nothing may be fetched at view time: the report has to open from an email
	// attachment on a machine with no network. Only constructs that actually
	// retrieve something count - the SVG namespace URI is an identifier, not a
	// resource, and is never dereferenced.
	for _, forbidden := range []string{
		`src="http`, `src='http`, `href="http`, `href='http`,
		`src="//`, `href="//`, "@import", "url(http", "fetch(",
		"XMLHttpRequest", "<link rel=\"stylesheet\"", "<script src",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("report retrieves something external via %q", forbidden)
		}
	}
	for _, want := range []string{
		"puffy-data",   // the embedded measurement
		"198.51.100.9", // the target made it in
		"--lat-ramp",   // the palettes
		"--loss-ramp",
		"Packet loss by hop over time",
		"Latency stalls by hop over time",
		"Hop statistics", // the table view twin
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

// The session is embedded inside a <script> element, so a hostname that
// contains a closing tag must not be able to break out of it.
func TestWriteHTMLEscapesEmbeddedData(t *testing.T) {
	s := testSession()
	s.Hops[0].Name = `</script><img src=x onerror=alert(1)>`

	path := filepath.Join(t.TempDir(), "xss.html")
	if err := WriteHTML(s, path, "auto"); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	body, _ := os.ReadFile(path)
	html := string(body)

	if strings.Contains(html, "</script><img") {
		t.Error("a hostname escaped the script element it was embedded in")
	}
	if !strings.Contains(html, `</script>`) {
		t.Error("the closing tag was not escaped as a unicode sequence")
	}
}

func TestWriteHTMLThemeStamp(t *testing.T) {
	for _, theme := range []string{"light", "dark", "auto", "nonsense"} {
		path := filepath.Join(t.TempDir(), "t.html")
		if err := WriteHTML(testSession(), path, theme); err != nil {
			t.Fatalf("theme %q: %v", theme, err)
		}
		body, _ := os.ReadFile(path)
		want := theme
		if theme == "nonsense" {
			want = "auto" // an unknown value falls back rather than erroring
		}
		if !strings.Contains(string(body), `data-theme="`+want+`"`) {
			t.Errorf("theme %q did not stamp data-theme=%q", theme, want)
		}
	}
}
