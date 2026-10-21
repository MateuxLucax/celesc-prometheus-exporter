// Package backfill writes the exporter's price metrics for a past period as an
// OpenMetrics file, for `promtool tsdb create-blocks-from openmetrics`. That
// lets a power history already in Prometheus (a Tapo plug, say) be priced with
// the tariff, flag and taxes that were in force at the time.
//
// Inputs only change daily at most, so values are resolved once per day and
// then repeated every Step. The step must stay under Prometheus' 5-minute
// lookback, or instant queries joining against these series find gaps.
package backfill

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/exporter"
)

// MaxStep is the largest step that still lines up with Prometheus' default
// 5-minute lookback.
const MaxStep = 4 * time.Minute

// Inputs is everything needed to price any day in the period.
type Inputs struct {
	Tarifas   aneel.TarifaHistory
	Bandeiras aneel.BandeiraHistory
	// Aliquotas is the Celesc PIS/COFINS table; nil means PIS/COFINS come from
	// Config.
	Aliquotas celesc.History
	Config    exporter.Config
}

// Options shape the output.
type Options struct {
	From, To time.Time
	Step     time.Duration
	// Labels are added to every series, normally job and instance so the
	// backfill joins the live scrape's series.
	Labels [][2]string
}

// Report says what was written.
type Report struct {
	Days, SkippedDays int
	Samples           int
}

type day struct {
	start  time.Time          // midnight in Config.Location
	end    time.Time          // the next day's start
	values map[string]float64 // series key (name + labels) → value
}

// Write resolves every day in [From, To) and writes the OpenMetrics document.
// Days whose inputs aren't published (before the Celesc table starts, say)
// are skipped and counted in the report.
func Write(w io.Writer, in Inputs, opt Options) (Report, error) {
	if opt.Step <= 0 || opt.Step > MaxStep {
		return Report{}, fmt.Errorf("step must be between 0 and %s", MaxStep)
	}
	if !opt.From.Before(opt.To) {
		return Report{}, errors.New("from must be before to")
	}
	loc := in.Config.Location
	if loc == nil {
		loc = time.UTC
	}

	var rep Report
	var days []day
	f := opt.From.In(loc)
	for i := 0; ; i++ {
		d := midnight(f.Year(), f.Month(), f.Day()+i, loc)
		if !d.Before(opt.To) {
			break
		}
		rep.Days++
		v, ok := resolve(in, d)
		if !ok {
			rep.SkippedDays++
			continue
		}
		v.start, v.end = d, midnight(f.Year(), f.Month(), f.Day()+i+1, loc)
		days = append(days, v)
	}
	if len(days) == 0 {
		return rep, errors.New("no day in the period has all inputs published")
	}

	// OpenMetrics wants each family's samples together and each series in
	// time order, so write series by series.
	keys := map[string]bool{}
	for _, d := range days {
		for k := range d.values {
			keys[k] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	extra := ""
	for _, l := range opt.Labels {
		extra += "," + l[0] + `="` + exporter.EscapeLabel(l[1]) + `"`
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	lastFamily := ""
	for _, key := range sorted {
		name, labels := splitKey(key)
		if name != lastFamily {
			fmt.Fprintf(bw, "# TYPE %s gauge\n", name)
			lastFamily = name
		}
		series := name + "{" + strings.TrimPrefix(labels+extra, ",") + "}"
		if labels == "" && extra == "" {
			series = name
		}
		for _, d := range days {
			v, ok := d.values[key]
			if !ok {
				continue
			}
			end := d.end
			if end.After(opt.To) {
				end = opt.To
			}
			val := strconv.FormatFloat(v, 'g', -1, 64)
			for t := d.start; t.Before(end); t = t.Add(opt.Step) {
				if t.Before(opt.From) {
					continue
				}
				bw.WriteString(series + " " + val + " " + strconv.FormatInt(t.Unix(), 10) + "\n")
				rep.Samples++
			}
		}
	}
	bw.WriteString("# EOF\n")
	return rep, bw.Flush()
}

// resolve computes one day's values, or ok=false if any input is missing.
// The histories' At falls back to the latest earlier row, which is right for
// the live exporter but would price old days with whatever came before, so
// each input must be the one of that exact day or month.
func resolve(in Inputs, d time.Time) (day, bool) {
	// Pick at noon so a timezone offset can never move the date.
	noon := d.Add(12 * time.Hour)
	date := time.Date(noon.Year(), noon.Month(), noon.Day(), 0, 0, 0, 0, time.UTC)
	t, err := in.Tarifas.At(noon)
	if err != nil || t.InicioVigencia.After(date) || t.FimVigencia.Before(date) {
		return day{}, false
	}
	b, err := in.Bandeiras.At(noon)
	if err != nil || !sameMonth(b.Competencia, noon) {
		return day{}, false
	}
	pis, cofins := in.Config.ManualPIS, in.Config.ManualCOFINS
	if in.Aliquotas != nil {
		a, err := in.Aliquotas.At(noon)
		if err != nil || !sameMonth(a.Referencia, noon) {
			return day{}, false
		}
		pis, cofins = a.PIS, a.COFINS
	}
	icms := in.Config.ICMSPercent
	return day{
		values: map[string]float64{
			`energia_tarifa_reais_kwh{componente="te"}`:                              t.TE,
			`energia_tarifa_reais_kwh{componente="tusd"}`:                            t.TUSD,
			`energia_tarifa_reais_kwh{componente="bandeira"}`:                        b.Adicional,
			`energia_imposto_ratio{imposto="icms"}`:                                  icms / 100,
			`energia_imposto_ratio{imposto="pis"}`:                                   pis / 100,
			`energia_imposto_ratio{imposto="cofins"}`:                                cofins / 100,
			`energia_bandeira_info{bandeira="` + exporter.EscapeLabel(b.Nome) + `"}`: 1,
			`energia_tarifa_final_reais_kwh`:                                         exporter.Final(t.TE+t.TUSD+b.Adicional, icms, pis, cofins),
		},
	}, true
}

// midnight returns the start of a date in loc (day may overflow the month).
// Where DST skips midnight (Brazil before 2019), Go would resolve 00:00 to
// 23:00 of the previous day, so the day starts at 01:00 instead.
func midnight(year int, month time.Month, day int, loc *time.Location) time.Time {
	want := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	t := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if t.Day() != want.Day() {
		t = time.Date(year, month, day, 1, 0, 0, 0, loc)
	}
	return t
}

func sameMonth(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month()
}

// splitKey turns `name{a="b"}` into ("name", `a="b"`).
func splitKey(key string) (name, labels string) {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		return key[:i], strings.TrimSuffix(key[i+1:], "}")
	}
	return key, ""
}
