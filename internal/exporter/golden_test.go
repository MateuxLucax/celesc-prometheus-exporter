package exporter

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
)

// The files in testdata are real responses captured on 2026-10-21: ANEEL's
// datastore_search for Celesc B1 residential tariffs and for the bandeira, and
// the PIS/COFINS table of the Celesc tariff page. These tests run the real
// clients against them end to end, so a change to parsing, row selection or
// the price formula shows up as a different R$/kWh.

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tarifas, bandeiras, celescPage := read("aneel-tarifas.json"), read("aneel-bandeiras.json"), read("celesc-tributos.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/celesc":
			w.Write(celescPage)
		case r.URL.Path == "/aneel/datastore_search" && r.URL.Query().Get("resource_id") == "fcf2906c-7c32-4b9b-a637-054e7a5234f4":
			w.Write(tarifas)
		case r.URL.Path == "/aneel/datastore_search" && r.URL.Query().Get("resource_id") == "0591b8f6-fe54-437b-b72b-1aa2efd46e42":
			w.Write(bandeiras)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixtureExporter(t *testing.T, now time.Time) *Exporter {
	srv := fixtureServer(t)
	ac := &aneel.Client{BaseURL: srv.URL + "/aneel/", HTTPClient: srv.Client(), AllowHTTP: true}
	cc := &celesc.Client{URL: srv.URL + "/celesc", HTTPClient: srv.Client(), AllowHTTP: true}
	f := aneel.DefaultFilter()
	e := New(Sources{
		Tarifas: func(ctx context.Context) (History[aneel.Tarifa], error) { return ac.Tarifas(ctx, f) },
		Bandeiras: func(ctx context.Context) (History[aneel.Bandeira], error) {
			return ac.Bandeiras(ctx, aneel.RecentBandeiras)
		},
		Aliquotas: func(ctx context.Context) (History[celesc.Aliquotas], error) { return cc.History(ctx) },
	}, Config{ICMSPercent: 17, Location: time.UTC})
	e.Now = func() time.Time { return now }
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

var sampleRe = regexp.MustCompile(`(?m)^(\S+) (\S+)$`)

// metrics parses a scrape into "name{labels}" → value.
func metrics(t *testing.T, e *Exporter) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range sampleRe.FindAllStringSubmatch(scrape(e), -1) {
		if strings.HasPrefix(m[1], "#") {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("bad sample %q", m[0])
		}
		out[m[1]] = v
	}
	return out
}

func TestGoldenPrices(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		now                    time.Time
		bandeira               string
		te, tusd               float64
		adicional, pis, cofins float64
		final                  float64
	}{
		{
			// Reajuste 2026 in force, green flag, October's PIS/COFINS.
			name: "outubro 2026", now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
			bandeira: "Verde", te: 0.34571, tusd: 0.41404, adicional: 0, pis: 0.0027, cofins: 0.0127,
			final: 0.9326663393076357,
		},
		{
			// The R$ 1.03 that was typed into the Grafana dashboard by hand.
			name: "agosto 2026 apos reajuste", now: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
			bandeira: "Amarela", te: 0.34571, tusd: 0.41404, adicional: 0.01885, pis: 0.0132, cofins: 0.0608,
			final: 1.02989417989418,
		},
		{
			// Before 22/08 the previous tariff (REH 3.511) is the one in force.
			name: "agosto 2026 antes do reajuste", now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
			bandeira: "Amarela", te: 0.32193, tusd: 0.37375, adicional: 0.01885, pis: 0.0132, cofins: 0.0608,
			final: 0.9451455026455028,
		},
		{
			// November isn't published anywhere yet: October's flag and taxes
			// carry over.
			name: "novembro 2026 sem publicacao", now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
			bandeira: "Verde", te: 0.34571, tusd: 0.41404, adicional: 0, pis: 0.0027, cofins: 0.0127,
			final: 0.9326663393076357,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics(t, fixtureExporter(t, tc.now))
			want := map[string]float64{
				`energia_tarifa_reais_kwh{componente="te"}`:             tc.te,
				`energia_tarifa_reais_kwh{componente="tusd"}`:           tc.tusd,
				`energia_tarifa_reais_kwh{componente="bandeira"}`:       tc.adicional,
				`energia_bandeira_info{bandeira="` + tc.bandeira + `"}`: 1,
				`energia_imposto_ratio{imposto="icms"}`:                 0.17,
				`energia_imposto_ratio{imposto="pis"}`:                  tc.pis,
				`energia_imposto_ratio{imposto="cofins"}`:               tc.cofins,
				`energia_tarifa_final_reais_kwh`:                        tc.final,
				`energia_fonte_sucesso{fonte="aneel_tarifa"}`:           1,
				`energia_fonte_sucesso{fonte="aneel_bandeira"}`:         1,
				`energia_fonte_sucesso{fonte="celesc_pis_cofins"}`:      1,
			}
			for k, v := range want {
				got, ok := m[k]
				if !ok {
					t.Errorf("missing %s", k)
				} else if math.Abs(got-v) > 1e-9 {
					t.Errorf("%s = %v, want %v", k, got, v)
				}
			}
		})
	}
}

// TestGoldenScrapeIsValidExposition checks every line of a real scrape is a
// comment or a well-formed sample, so Prometheus can always parse it.
func TestGoldenScrapeIsValidExposition(t *testing.T) {
	e := fixtureExporter(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	line := regexp.MustCompile(`^(# (HELP|TYPE) [a-z_]+ .+|[a-z_]+(\{[a-z_]+="[^"\\\n]*"\})? -?[0-9.e+]+)$`)
	for _, l := range strings.Split(strings.TrimSuffix(scrape(e), "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("malformed line %q", l)
		}
	}
}

// TestScrapeUsesMonthInForce refreshes on the last evening of August and
// scrapes after midnight: September's PIS/COFINS apply at once, without
// waiting for the next refresh.
func TestScrapeUsesMonthInForce(t *testing.T) {
	e := fixtureExporter(t, time.Date(2026, 8, 31, 22, 0, 0, 0, time.UTC))
	e.Now = func() time.Time { return time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC) }
	m := metrics(t, e)
	if got := m[`energia_imposto_ratio{imposto="pis"}`]; math.Abs(got-0.0089) > 1e-12 {
		t.Errorf("pis = %v, want September's 0.0089", got)
	}
	if got := m[`energia_imposto_ratio{imposto="cofins"}`]; math.Abs(got-0.0412) > 1e-12 {
		t.Errorf("cofins = %v, want September's 0.0412", got)
	}
}
