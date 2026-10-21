package exporter

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
)

type fakes struct {
	tarifa    aneel.Tarifa
	bandeira  aneel.Bandeira
	aliquotas celesc.Aliquotas
	err       error // returned by every source when set
}

func (f *fakes) sources(withCelesc bool) Sources {
	s := Sources{
		Tarifa:   func(context.Context, time.Time) (aneel.Tarifa, error) { return f.tarifa, f.err },
		Bandeira: func(context.Context, time.Time) (aneel.Bandeira, error) { return f.bandeira, f.err },
	}
	if withCelesc {
		s.Aliquotas = func(context.Context, time.Time) (celesc.Aliquotas, error) { return f.aliquotas, f.err }
	}
	return s
}

func scrape(e *Exporter) string {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestFinal(t *testing.T) {
	// 0.75975 R$/kWh with ICMS 17% + PIS 0.27% + COFINS 1.27%.
	got := Final(0.75975, 17, 0.27, 1.27)
	if math.Abs(got-0.75975/0.8146) > 1e-12 {
		t.Fatalf("got %v", got)
	}
}

func TestScrapeAfterRefresh(t *testing.T) {
	f := &fakes{
		tarifa:    aneel.Tarifa{TE: 0.34571, TUSD: 0.41404, InicioVigencia: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)},
		bandeira:  aneel.Bandeira{Nome: "Amarela", Adicional: 0.01885},
		aliquotas: celesc.Aliquotas{PIS: 0.27, COFINS: 1.27},
	}
	e := New(f.sources(true), Config{ICMSPercent: 17, COSIPReaisMes: 25.5})
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := scrape(e)
	for _, want := range []string{
		`energia_tarifa_reais_kwh{componente="te"} 0.34571`,
		`energia_tarifa_reais_kwh{componente="bandeira"} 0.01885`,
		`energia_bandeira_info{bandeira="Amarela"} 1`,
		`energia_imposto_ratio{imposto="icms"} 0.17`,
		`energia_imposto_ratio{imposto="cofins"} 0.0127`,
		`energia_cosip_reais_mes 25.5`,
		`energia_fonte_sucesso{fonte="celesc_pis_cofins"} 1`,
		"energia_tarifa_final_reais_kwh ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestColdStartFailureExportsNoPrice(t *testing.T) {
	f := &fakes{err: errors.New("down")}
	e := New(f.sources(true), Config{ICMSPercent: 17})
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("want error")
	}
	out := scrape(e)
	if strings.Contains(out, "energia_tarifa_final_reais_kwh") || strings.Contains(out, "energia_tarifa_reais_kwh") {
		t.Fatalf("price exported from no data:\n%s", out)
	}
	if !strings.Contains(out, `energia_fonte_sucesso{fonte="aneel_tarifa"} 0`) {
		t.Fatalf("missing failure gauge:\n%s", out)
	}
}

func TestFailureKeepsLastGoodValue(t *testing.T) {
	f := &fakes{tarifa: aneel.Tarifa{TE: 0.3, TUSD: 0.4}, bandeira: aneel.Bandeira{Nome: "Verde"}}
	e := New(f.sources(false), Config{ICMSPercent: 17, ManualPIS: 1, ManualCOFINS: 4})
	_ = e.Refresh(context.Background())
	f.err = errors.New("down")
	_ = e.Refresh(context.Background())

	out := scrape(e)
	if !strings.Contains(out, `energia_tarifa_reais_kwh{componente="te"} 0.3`) {
		t.Fatalf("lost last good value:\n%s", out)
	}
	if !strings.Contains(out, `energia_fonte_sucesso{fonte="aneel_tarifa"} 0`) {
		t.Fatalf("failure not flagged:\n%s", out)
	}
	if strings.Contains(out, "celesc_pis_cofins") {
		t.Fatalf("manual PIS/COFINS should not report a celesc source:\n%s", out)
	}
	if !strings.Contains(out, `energia_imposto_ratio{imposto="pis"} 0.01`) {
		t.Fatalf("manual PIS missing:\n%s", out)
	}
}

func TestLabelEscaping(t *testing.T) {
	f := &fakes{tarifa: aneel.Tarifa{TE: 0.3, TUSD: 0.4}, bandeira: aneel.Bandeira{Nome: "x\"} 1\nevil_metric 1\n" + strings.Repeat("a", 200)}}
	e := New(f.sources(false), Config{})
	_ = e.Refresh(context.Background())
	out := scrape(e)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "evil_metric") {
			t.Fatalf("label value broke out of the sample:\n%s", out)
		}
	}
	if !strings.Contains(out, `bandeira="x\"} 1\nevil_metric 1\n`) {
		t.Fatalf("label not escaped:\n%s", out)
	}
}

func TestNoHealthBeforeFirstRefresh(t *testing.T) {
	e := New((&fakes{}).sources(true), Config{ICMSPercent: 17})
	if out := scrape(e); strings.Contains(out, "energia_fonte_sucesso{") {
		t.Fatalf("health reported before any refresh:\n%s", out)
	}
}
