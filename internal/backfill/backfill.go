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
	// CelescPage is the raw tariff page; nil means PIS/COFINS come from Config.
	CelescPage []byte
	Config     exporter.Config
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
	for d := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc); d.Before(opt.To); d = d.AddDate(0, 0, 1) {
		rep.Days++
		v, ok := resolve(in, d)
		if !ok {
			rep.SkippedDays++
			continue
		}
		v.start = d
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
		for i, d := range days {
			v, ok := d.values[key]
			if !ok {
				continue
			}
			end := d.start.AddDate(0, 0, 1)
			if i+1 < len(days) && days[i+1].start.Before(end) {
				end = days[i+1].start
			}
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
func resolve(in Inputs, d time.Time) (day, bool) {
	// Pick at noon so a timezone offset can never move the date.
	noon := d.Add(12 * time.Hour)
	t, err := in.Tarifas.At(noon)
	if err != nil {
		return day{}, false
	}
	b, err := in.Bandeiras.At(noon)
	if err != nil {
		return day{}, false
	}
	pis, cofins := in.Config.ManualPIS, in.Config.ManualCOFINS
	if in.CelescPage != nil {
		a, err := celesc.Parse(in.CelescPage, noon)
		if err != nil {
			return day{}, false
		}
		// Parse falls back to the latest earlier month; for history that
		// would price old months with whatever came before, so require the
		// exact month.
		if a.Referencia.Year() != noon.Year() || a.Referencia.Month() != noon.Month() {
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

// splitKey turns `name{a="b"}` into ("name", `a="b"`).
func splitKey(key string) (name, labels string) {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		return key[:i], strings.TrimSuffix(key[i+1:], "}")
	}
	return key, ""
}
