package backfill

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
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
	var err error
	if in.Aliquotas, err = celesc.ParseAll(read("celesc-tributos.html")); err != nil {
		t.Fatal(err)
	}
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

func TestWriteRequiresFlagOfTheMonth(t *testing.T) {
	// Manual PIS/COFINS, so only the flag decides: November isn't published
	// in the fixture and must be skipped, not priced with October's flag.
	in := fixtures(t)
	in.Aliquotas = nil
	in.Config.ManualPIS, in.Config.ManualCOFINS = 0.27, 1.27
	rep, err := Write(&bytes.Buffer{}, in, Options{
		From: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Step: 4 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 2 || rep.SkippedDays != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestWriteAcrossMissingMidnight(t *testing.T) {
	// Chile's DST starts at 24:00 on 2026-09-05, so 2026-09-06 00:00 doesn't
	// exist there. Later days must still start at midnight: October's
	// PIS/COFINS apply from 01/10 00:00, not an hour early or late.
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip(err)
	}
	in := fixtures(t)
	in.Config.Location = loc
	var buf bytes.Buffer
	if _, err := Write(&buf, in, Options{
		From: time.Date(2026, 9, 5, 0, 0, 0, 0, loc), To: time.Date(2026, 10, 2, 0, 0, 0, 0, loc), Step: 4 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	pis := parse(t, buf.String())[`energia_imposto_ratio{imposto="pis"}`]
	for _, ts := range []time.Time{
		time.Date(2026, 9, 5, 23, 56, 0, 0, loc), // last sample of 05/09
		time.Date(2026, 9, 6, 1, 0, 0, 0, loc),   // first instant of 06/09
		time.Date(2026, 9, 6, 12, 0, 0, 0, loc),
	} {
		if _, ok := pis[ts.Unix()]; !ok {
			t.Errorf("no sample at %s", ts)
		}
	}
	if got := pis[time.Date(2026, 9, 30, 23, 56, 0, 0, loc).Unix()]; got != 0.0089 {
		t.Errorf("pis at 30/09 23:56 = %v, want September's 0.0089", got)
	}
	if got := pis[time.Date(2026, 10, 1, 0, 0, 0, 0, loc).Unix()]; got != 0.0027 {
		t.Errorf("pis at 01/10 00:00 = %v, want October's 0.0027", got)
	}
}

func TestFixtureRowsWithNbsp(t *testing.T) {
	// The real page writes "0,07%&nbsp;" in some rows; they must not be lost.
	h := fixtures(t).Aliquotas
	for _, m := range []time.Time{time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)} {
		a, err := h.At(m)
		if err != nil || !a.Referencia.Equal(m) {
			t.Errorf("%s: got %+v, %v", m.Format("01/2006"), a, err)
		}
	}
}
