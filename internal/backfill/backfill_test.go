package backfill

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/exporter"
)

// Reuses the real responses captured for the exporter's golden tests.
func fixtures(t *testing.T) Inputs {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("../exporter/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var tar struct {
		Result struct{ Records json.RawMessage } `json:"result"`
	}
	var ban = tar
	if err := json.Unmarshal(read("aneel-tarifas.json"), &tar); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read("aneel-bandeiras.json"), &ban); err != nil {
		t.Fatal(err)
	}
	var in Inputs
	if err := json.Unmarshal(tar.Result.Records, &in.Tarifas); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ban.Result.Records, &in.Bandeiras); err != nil {
		t.Fatal(err)
	}
	in.CelescPage = read("celesc-tributos.html")
	in.Config = exporter.Config{ICMSPercent: 17, Location: time.UTC}
	return in
}

// valuesAt parses the document into series → (unix ts → value).
func parse(t *testing.T, doc string) map[string]map[int64]float64 {
	t.Helper()
	out := map[string]map[int64]float64{}
	lines := strings.Split(strings.TrimSuffix(doc, "\n"), "\n")
	if lines[len(lines)-1] != "# EOF" {
		t.Fatal("missing # EOF")
	}
	seenFamily := map[string]bool{}
	family := ""
	for _, l := range lines[:len(lines)-1] {
		if strings.HasPrefix(l, "# TYPE ") {
			family = strings.Fields(l)[2]
			if seenFamily[family] {
				t.Fatalf("family %s split across the document", family)
			}
			seenFamily[family] = true
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		j := strings.LastIndexByte(l[:i], ' ')
		series := l[:j]
		if !strings.HasPrefix(series, family) {
			t.Fatalf("sample %q outside its family %s", l, family)
		}
		v, err1 := strconv.ParseFloat(l[j+1:i], 64)
		ts, err2 := strconv.ParseInt(l[i+1:], 10, 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("bad line %q", l)
		}
		if out[series] == nil {
			out[series] = map[int64]float64{}
		}
		out[series][ts] = v
	}
	return out
}

func TestWriteAcrossReajuste(t *testing.T) {
	var buf bytes.Buffer
	from := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	rep, err := Write(&buf, fixtures(t), Options{
		From: from, To: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), Step: 4 * time.Minute,
		Labels: [][2]string{{"job", "energia"}, {"instance", "x:3000"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 4 || rep.SkippedDays != 0 || rep.Samples != 4*360*8 {
		t.Fatalf("report %+v", rep)
	}
	m := parse(t, buf.String())
	final := m[`energia_tarifa_final_reais_kwh{job="energia",instance="x:3000"}`]
	at := func(d time.Time) float64 { return final[d.Unix()] }

	// 21/08 still has REH 3.511; 22/08 has REH 3.602. Both with August's
	// Amarela flag and PIS/COFINS.
	if got := at(time.Date(2026, 8, 21, 23, 56, 0, 0, time.UTC)); got != 0.9451455026455028 {
		t.Errorf("21/08 = %v", got)
	}
	if got := at(time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)); got != 1.02989417989418 {
		t.Errorf("22/08 = %v", got)
	}
	if len(final) != 4*360 {
		t.Errorf("%d samples, want one every 4m", len(final))
	}
	if _, ok := m[`energia_bandeira_info{bandeira="Amarela",job="energia",instance="x:3000"}`]; !ok {
		t.Error("missing bandeira_info")
	}
}

func TestWriteSkipsUnpublishedMonths(t *testing.T) {
	// The flag fixture only goes back to 05/2026 (the last six months), so
	// April has no flag: it is skipped, not priced with May's numbers.
	in := fixtures(t)
	var buf bytes.Buffer
	rep, err := Write(&buf, in, Options{
		From: time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), Step: 4 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 3 || rep.SkippedDays != 2 {
		t.Fatalf("report %+v", rep)
	}
}

func TestWriteRejects(t *testing.T) {
	in := fixtures(t)
	d := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for name, opt := range map[string]Options{
		"step too big":      {From: d, To: d.Add(time.Hour), Step: 5 * time.Minute},
		"zero step":         {From: d, To: d.Add(time.Hour)},
		"empty range":       {From: d, To: d, Step: time.Minute},
		"nothing published": {From: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2001, 1, 3, 0, 0, 0, 0, time.UTC), Step: time.Minute},
	} {
		if _, err := Write(&bytes.Buffer{}, in, opt); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
