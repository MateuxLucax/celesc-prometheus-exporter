// Package exporter turns the upstream tariff data into Prometheus metrics. It
// refreshes every source in the background on an interval, keeps the last good
// history of each one, and picks the values in force at scrape time — ANEEL
// and Celesc are slow and change at most monthly, so scrapes never wait on
// them, and a month or reajuste that starts between refreshes shows up at once.
//
// Like the rest of the module it depends only on the standard library and
// writes the Prometheus text exposition format directly.
package exporter

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
)

// DefaultRefreshTimeout bounds one refresh of all sources.
const DefaultRefreshTimeout = 2 * time.Minute

// History picks the value in force at a given time. aneel.TarifaHistory,
// aneel.BandeiraHistory and celesc.History implement it.
type History[T any] interface {
	At(now time.Time) (T, error)
}

// Sources is what a refresh reads from: each returns the whole published
// history, so the value in force can be picked at scrape time. Tests use fakes.
type Sources struct {
	Tarifas   func(ctx context.Context) (History[aneel.Tarifa], error)
	Bandeiras func(ctx context.Context) (History[aneel.Bandeira], error)
	Aliquotas func(ctx context.Context) (History[celesc.Aliquotas], error) // nil when PIS/COFINS are manual
}

// Config holds the values that don't come from any upstream.
type Config struct {
	ICMSPercent float64
	// ManualPIS/ManualCOFINS are used when Sources.Aliquotas is nil.
	ManualPIS    float64
	ManualCOFINS float64
	// COSIPReaisMes is the monthly street-lighting fee; 0 leaves the metric out.
	COSIPReaisMes float64
	// Location decides which calendar month "now" is in.
	Location *time.Location
}

type source[T any] struct {
	history     History[T]
	ok          bool    // a history was fetched at least once
	attempted   bool    // a refresh has finished at least once
	success     float64 // 1 if the last attempt succeeded
	lastSuccess float64 // unix seconds
}

// update stores h unless the fetch failed or h has nothing usable for now, so
// a format change upstream shows up as a failed refresh, not a wrong price.
func (s *source[T]) update(h History[T], err error, now time.Time) error {
	s.attempted = true
	if err == nil {
		_, err = h.At(now)
	}
	if err != nil {
		s.success = 0
		return err
	}
	s.history, s.ok, s.success, s.lastSuccess = h, true, 1, float64(now.Unix())
	return nil
}

// at returns the value in force at now, if a history is known and has one.
func (s *source[T]) at(now time.Time) (T, bool) {
	var zero T
	if !s.ok {
		return zero, false
	}
	v, err := s.history.At(now)
	if err != nil {
		return zero, false
	}
	return v, true
}

// Exporter caches the latest data and serves it as Prometheus metrics. It
// implements http.Handler for the /metrics endpoint.
type Exporter struct {
	src Sources
	cfg Config

	// Now is the clock; tests override it.
	Now func() time.Time

	refreshMu sync.Mutex // single-flight for Refresh

	mu        sync.RWMutex // guards the three sources
	tarifa    source[aneel.Tarifa]
	bandeira  source[aneel.Bandeira]
	aliquotas source[celesc.Aliquotas]
}

// New builds an Exporter.
func New(src Sources, cfg Config) *Exporter {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	return &Exporter{src: src, cfg: cfg, Now: time.Now}
}

// Refresh reads every source once. A failing source keeps its last good value
// and only flips its success gauge to 0; the returned error joins all failures.
func (e *Exporter) Refresh(ctx context.Context) error {
	if !e.refreshMu.TryLock() {
		return nil
	}
	defer e.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, DefaultRefreshTimeout)
	defer cancel()
	now := e.Now().In(e.cfg.Location)

	// The sources are independent, so a slow one doesn't eat into the
	// others' share of the timeout.
	var (
		wg               sync.WaitGroup
		t                History[aneel.Tarifa]
		b                History[aneel.Bandeira]
		a                History[celesc.Aliquotas]
		tErr, bErr, aErr error
	)
	wg.Go(func() { t, tErr = e.src.Tarifas(ctx) })
	wg.Go(func() { b, bErr = e.src.Bandeiras(ctx) })
	if e.src.Aliquotas != nil {
		wg.Go(func() { a, aErr = e.src.Aliquotas(ctx) })
	}
	wg.Wait()

	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []string
	if err := e.tarifa.update(t, tErr, now); err != nil {
		errs = append(errs, "tarifa: "+err.Error())
	}
	if err := e.bandeira.update(b, bErr, now); err != nil {
		errs = append(errs, "bandeira: "+err.Error())
	}
	if e.src.Aliquotas != nil {
		if err := e.aliquotas.update(a, aErr, now); err != nil {
			errs = append(errs, "pis/cofins: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Start refreshes once immediately, then on every interval tick until ctx is
// cancelled. It blocks.
func (e *Exporter) Start(ctx context.Context, interval time.Duration) {
	e.refreshAndLog(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshAndLog(ctx)
		}
	}
}

func (e *Exporter) refreshAndLog(ctx context.Context) {
	if err := e.Refresh(ctx); err != nil && ctx.Err() == nil {
		log.Printf("refresh failed: %v", err)
	}
}

// ServeHTTP writes the metrics in force now in the Prometheus text exposition
// format. Price metrics are left out until every input they need is known, so
// a cold start never exports a price computed from zeros.
func (e *Exporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	now := e.Now().In(e.cfg.Location)
	t, tarifaOK := e.tarifa.at(now)
	b, bandeiraOK := e.bandeira.at(now)
	a, aliquotasOK := e.aliquotas.at(now)
	pis, cofins, taxesOK := a.PIS, a.COFINS, aliquotasOK
	if e.src.Aliquotas == nil {
		pis, cofins, taxesOK = e.cfg.ManualPIS, e.cfg.ManualCOFINS, true
	}

	if tarifaOK || bandeiraOK {
		writeHeader(w, "energia_tarifa_reais_kwh", "Tariff component in R$/kWh, without taxes")
	}
	if tarifaOK {
		writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "te"}}, t.TE)
		writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "tusd"}}, t.TUSD)
	}
	if bandeiraOK {
		writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "bandeira"}}, b.Adicional)
	}
	if tarifaOK {
		writeGauge(w, "energia_tarifa_vigencia_inicio_timestamp_seconds", "Start of the tariff's validity (Unix seconds)", float64(t.InicioVigencia.Unix()))
		writeGauge(w, "energia_tarifa_vigencia_fim_timestamp_seconds", "End of the tariff's validity (Unix seconds)", float64(t.FimVigencia.Unix()))
	}

	if bandeiraOK {
		writeHeader(w, "energia_bandeira_info", "Tariff flag in force; the value is always 1")
		writeSample(w, "energia_bandeira_info", [][2]string{{"bandeira", b.Nome}}, 1)
		writeGauge(w, "energia_bandeira_competencia_timestamp_seconds", "Month the flag refers to (Unix seconds)", float64(b.Competencia.Unix()))
	}

	writeHeader(w, "energia_imposto_ratio", "Tax rate applied on top of the tariff, as a ratio")
	writeSample(w, "energia_imposto_ratio", [][2]string{{"imposto", "icms"}}, e.cfg.ICMSPercent/100)
	if taxesOK {
		writeSample(w, "energia_imposto_ratio", [][2]string{{"imposto", "pis"}}, pis/100)
		writeSample(w, "energia_imposto_ratio", [][2]string{{"imposto", "cofins"}}, cofins/100)
	}
	if aliquotasOK {
		writeGauge(w, "energia_pis_cofins_referencia_timestamp_seconds", "Month the PIS/COFINS rates refer to (Unix seconds)", float64(a.Referencia.Unix()))
	}

	if tarifaOK && bandeiraOK && taxesOK {
		writeGauge(w, "energia_tarifa_final_reais_kwh", "Price per kWh with flag and taxes (ICMS, PIS, COFINS), in R$", Final(t.TE+t.TUSD+b.Adicional, e.cfg.ICMSPercent, pis, cofins))
	}

	if e.cfg.COSIPReaisMes > 0 {
		writeGauge(w, "energia_cosip_reais_mes", "Monthly street-lighting fee (COSIP/CIP), in R$", e.cfg.COSIPReaisMes)
	}

	// Health is only reported once a source has been tried, so the seconds
	// before the first refresh finishes don't read as "source down".
	type health struct {
		name                 string
		attempted            bool
		success, lastSuccess float64
	}
	hs := []health{
		{"aneel_tarifa", e.tarifa.attempted, e.tarifa.success, e.tarifa.lastSuccess},
		{"aneel_bandeira", e.bandeira.attempted, e.bandeira.success, e.bandeira.lastSuccess},
	}
	if e.src.Aliquotas != nil {
		hs = append(hs, health{"celesc_pis_cofins", e.aliquotas.attempted, e.aliquotas.success, e.aliquotas.lastSuccess})
	}
	writeHeader(w, "energia_fonte_sucesso", "1 if the last refresh of the source succeeded, 0 otherwise")
	for _, h := range hs {
		if h.attempted {
			writeSample(w, "energia_fonte_sucesso", [][2]string{{"fonte", h.name}}, h.success)
		}
	}
	writeHeader(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", "Unix time of the source's last successful refresh")
	for _, h := range hs {
		if h.attempted && h.lastSuccess > 0 {
			writeSample(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", [][2]string{{"fonte", h.name}}, h.lastSuccess)
		}
	}
}

// Final grosses a tax-free price up by ICMS, PIS and COFINS, all in percent.
// Brazilian energy taxes are charged "por dentro" (each is a share of the
// final price), so the price is divided by (1 - total rate), not multiplied.
func Final(base, icms, pis, cofins float64) float64 {
	return base / (1 - (icms+pis+cofins)/100)
}

func writeGauge(w io.Writer, name, help string, value float64) {
	writeHeader(w, name, help)
	writeSample(w, name, nil, value)
}

func writeHeader(w io.Writer, name, help string) {
	io.WriteString(w, "# HELP "+name+" "+help+"\n")
	io.WriteString(w, "# TYPE "+name+" gauge\n")
}

func writeSample(w io.Writer, name string, labels [][2]string, value float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l[0])
			b.WriteString(`="`)
			b.WriteString(EscapeLabel(l[1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

// maxLabelLen bounds label values that come from upstream data.
const maxLabelLen = 64

// EscapeLabel escapes a label value per the text exposition format and
// truncates it, since the flag name comes from ANEEL and must not be able to
// break out of the label or bloat the series.
func EscapeLabel(s string) string {
	if r := []rune(s); len(r) > maxLabelLen {
		s = string(r[:maxLabelLen])
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", "").Replace(s)
}
