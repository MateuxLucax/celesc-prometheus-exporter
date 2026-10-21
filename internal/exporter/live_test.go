package exporter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
)

// TestLiveSources hits the real ANEEL API and Celesc page. It is the contract
// test for upstream format changes (a renamed column, a new resource ID, a
// reworked HTML table), which the fixture tests can't see. It only runs with
// LIVE_TESTS=1; CI runs it on a weekly schedule.
func TestLiveSources(t *testing.T) {
	if os.Getenv("LIVE_TESTS") != "1" {
		t.Skip("set LIVE_TESTS=1 to hit the real ANEEL and Celesc endpoints")
	}
	ac, cc := aneel.NewClient(), celesc.NewClient()
	f := aneel.DefaultFilter()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	e := New(Sources{
		Tarifas: func(ctx context.Context) (History[aneel.Tarifa], error) { return ac.Tarifas(ctx, f) },
		Bandeiras: func(ctx context.Context) (History[aneel.Bandeira], error) {
			return ac.Bandeiras(ctx, aneel.RecentBandeiras)
		},
		Aliquotas: func(ctx context.Context) (History[celesc.Aliquotas], error) { return cc.History(ctx) },
	}, Config{ICMSPercent: 17, Location: loc})

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	tarifa, ok1 := e.tarifa.at(now)
	bandeira, ok2 := e.bandeira.at(now)
	aliquotas, ok3 := e.aliquotas.at(now)
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("a source has nothing in force now")
	}

	// Celesc's tariff is renewed every August; an expired one means ANEEL
	// stopped publishing or the filter no longer matches the new rows.
	if tarifa.FimVigencia.Before(month) {
		t.Errorf("tariff expired on %s", tarifa.FimVigencia.Format(time.DateOnly))
	}
	// Both monthly sources publish around the start of the month; more than
	// two months behind means the fallback is masking a broken source.
	limit := month.AddDate(0, -2, 0)
	if bandeira.Competencia.Before(limit) {
		t.Errorf("bandeira stuck at %s", bandeira.Competencia.Format("01/2006"))
	}
	if aliquotas.Referencia.Before(limit) {
		t.Errorf("PIS/COFINS stuck at %s", aliquotas.Referencia.Format("01/2006"))
	}

	m := metrics(t, e)
	final := m["energia_tarifa_final_reais_kwh"]
	if final < 0.5 || final > 2.5 {
		t.Errorf("final price %v R$/kWh is outside any plausible range", final)
	}
	t.Logf("R$ %.4f/kWh (bandeira %s, PIS %.2f%%, COFINS %.2f%%)", final, bandeira.Nome, aliquotas.PIS, aliquotas.COFINS)
}
