// Package exporter turns the upstream tariff data into Prometheus metrics. It
// refreshes every source in the background on an interval, keeps the last good
// value of each one, and serves the cached snapshot on every scrape — ANEEL
// and Celesc are slow and change at most monthly, so scrapes never wait on
// them.
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

	"github.com/MateuxLucax/energia-tarifa-exporter/internal/aneel"
	"github.com/MateuxLucax/energia-tarifa-exporter/internal/celesc"
)

// DefaultRefreshTimeout bounds one refresh of all sources.
const DefaultRefreshTimeout = 2 * time.Minute

// Sources is what a refresh reads from. *aneel.Client and *celesc.Client
// satisfy the two halves; tests use fakes.
type Sources struct {
	Tarifa    func(ctx context.Context, now time.Time) (aneel.Tarifa, error)
	Bandeira  func(ctx context.Context, now time.Time) (aneel.Bandeira, error)
	Aliquotas func(ctx context.Context, now time.Time) (celesc.Aliquotas, error) // nil when PIS/COFINS are manual
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
	value       T
	ok          bool    // a value was fetched at least once
	success     float64 // 1 if the last attempt succeeded
	lastSuccess float64 // unix seconds
}

func (s *source[T]) update(v T, err error, now time.Time) error {
	if err != nil {
		s.success = 0
		return err
	}
	s.value, s.ok, s.success, s.lastSuccess = v, true, 1, float64(now.Unix())
	return nil
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

	t, tErr := e.src.Tarifa(ctx, now)
	b, bErr := e.src.Bandeira(ctx, now)
	var a celesc.Aliquotas
	var aErr error
	if e.src.Aliquotas != nil {
		a, aErr = e.src.Aliquotas(ctx, now)
	}

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

// pisCofins returns the rates in use and whether they are known.
func (e *Exporter) pisCofins() (pis, cofins float64, ok bool) {
	if e.src.Aliquotas == nil {
		return e.cfg.ManualPIS, e.cfg.ManualCOFINS, true
	}
	return e.aliquotas.value.PIS, e.aliquotas.value.COFINS, e.aliquotas.ok
}

// ServeHTTP writes the cached metrics in the Prometheus text exposition format.
// Price metrics are left out until every input they need has been fetched
// once, so a cold start never exports a price computed from zeros.
func (e *Exporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	pis, cofins, taxesOK := e.pisCofins()

	if e.tarifa.ok {
		t := e.tarifa.value
		writeHeader(w, "energia_tarifa_reais_kwh", "Tariff component in R$/kWh, without taxes")
		writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "te"}}, t.TE)
		writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "tusd"}}, t.TUSD)
		if e.bandeira.ok {
			writeSample(w, "energia_tarifa_reais_kwh", [][2]string{{"componente", "bandeira"}}, e.bandeira.value.Adicional)
		}
		writeGauge(w, "energia_tarifa_vigencia_inicio_timestamp_seconds", "Start of the tariff's validity (Unix seconds)", float64(t.InicioVigencia.Unix()))
		writeGauge(w, "energia_tarifa_vigencia_fim_timestamp_seconds", "End of the tariff's validity (Unix seconds)", float64(t.FimVigencia.Unix()))
	}

	if e.bandeira.ok {
		b := e.bandeira.value
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
	if e.src.Aliquotas != nil && e.aliquotas.ok {
		writeGauge(w, "energia_pis_cofins_referencia_timestamp_seconds", "Month the PIS/COFINS rates refer to (Unix seconds)", float64(e.aliquotas.value.Referencia.Unix()))
	}

	if e.tarifa.ok && e.bandeira.ok && taxesOK {
		base := e.tarifa.value.TE + e.tarifa.value.TUSD + e.bandeira.value.Adicional
		writeGauge(w, "energia_tarifa_final_reais_kwh", "Price per kWh with flag and taxes (ICMS, PIS, COFINS), in R$", Final(base, e.cfg.ICMSPercent, pis, cofins))
	}

	if e.cfg.COSIPReaisMes > 0 {
		writeGauge(w, "energia_cosip_reais_mes", "Monthly street-lighting fee (COSIP/CIP), in R$", e.cfg.COSIPReaisMes)
	}

	writeHeader(w, "energia_fonte_sucesso", "1 if the last refresh of the source succeeded, 0 otherwise")
	writeSample(w, "energia_fonte_sucesso", [][2]string{{"fonte", "aneel_tarifa"}}, e.tarifa.success)
	writeSample(w, "energia_fonte_sucesso", [][2]string{{"fonte", "aneel_bandeira"}}, e.bandeira.success)
	if e.src.Aliquotas != nil {
		writeSample(w, "energia_fonte_sucesso", [][2]string{{"fonte", "celesc_pis_cofins"}}, e.aliquotas.success)
	}
	writeHeader(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", "Unix time of the source's last successful refresh")
	writeSample(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", [][2]string{{"fonte", "aneel_tarifa"}}, e.tarifa.lastSuccess)
	writeSample(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", [][2]string{{"fonte", "aneel_bandeira"}}, e.bandeira.lastSuccess)
	if e.src.Aliquotas != nil {
		writeSample(w, "energia_fonte_ultimo_sucesso_timestamp_seconds", [][2]string{{"fonte", "celesc_pis_cofins"}}, e.aliquotas.lastSuccess)
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
			b.WriteString(escapeLabel(l[1]))
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

// escapeLabel escapes a label value per the text exposition format and
// truncates it, since the flag name comes from ANEEL and must not be able to
// break out of the label or bloat the series.
func escapeLabel(s string) string {
	if r := []rune(s); len(r) > maxLabelLen {
		s = string(r[:maxLabelLen])
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", "").Replace(s)
}
