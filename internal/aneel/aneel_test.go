package aneel

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPickTarifaInForce(t *testing.T) {
	recs := []tarifaRecord{
		// SCEE row of the same class: a different product, must be skipped.
		{DatInicioVigencia: "2026-08-22", DatFimVigencia: "2027-08-21", DscDetalhe: "SCEE", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "414,04", VlrTE: "40,43"},
		{DscREH: "REH 3.602", DatInicioVigencia: "2026-08-22", DatFimVigencia: "2027-08-21", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "414,04", VlrTE: "345,71"},
		{DatInicioVigencia: "2025-08-22", DatFimVigencia: "2026-08-21", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "400,00", VlrTE: "300,00"},
	}
	got, err := pickTarifa(recs, day("2026-10-21"))
	if err != nil {
		t.Fatal(err)
	}
	if !near(got.TE, 0.34571) || !near(got.TUSD, 0.41404) || got.Resolucao != "REH 3.602" {
		t.Fatalf("got %+v", got)
	}

	// Before the 2026 reajuste, the 2025 row is the one in force.
	got, err = pickTarifa(recs, day("2026-08-01"))
	if err != nil {
		t.Fatal(err)
	}
	if !near(got.TE, 0.3) {
		t.Fatalf("got %+v", got)
	}
}

func TestPickTarifaExpiredFallsBackToLatest(t *testing.T) {
	recs := []tarifaRecord{
		{DatInicioVigencia: "2025-08-22", DatFimVigencia: "2026-08-21", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "400,00", VlrTE: "300,00"},
	}
	got, err := pickTarifa(recs, day("2026-09-01"))
	if err != nil || !near(got.TUSD, 0.4) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPickTarifaRejects(t *testing.T) {
	for name, r := range map[string]tarifaRecord{
		"kW unit":     {DatInicioVigencia: "2026-01-01", DatFimVigencia: "2027-01-01", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "kW", VlrTUSD: "1,00", VlrTE: "1,00"},
		"future":      {DatInicioVigencia: "2027-01-01", DatFimVigencia: "2028-01-01", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "1,00", VlrTE: "1,00"},
		"not number":  {DatInicioVigencia: "2026-01-01", DatFimVigencia: "2027-01-01", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "NaN", VlrTE: "1,00"},
		"ponta posto": {DatInicioVigencia: "2026-01-01", DatFimVigencia: "2027-01-01", DscDetalhe: "Não se aplica", NomPostoTarifario: "Ponta", Unidade: "MWh", VlrTUSD: "1,00", VlrTE: "1,00"},
	} {
		if _, err := pickTarifa([]tarifaRecord{r}, day("2026-10-21")); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// 50 R$/kWh is not a residential tariff: likely a unit change upstream.
	r := tarifaRecord{DatInicioVigencia: "2026-01-01", DatFimVigencia: "2027-01-01", DscDetalhe: "Não se aplica", NomPostoTarifario: "Não se aplica", Unidade: "MWh", VlrTUSD: "50.000,00", VlrTE: "0,00"}
	if _, err := pickTarifa([]tarifaRecord{r}, day("2026-10-21")); err == nil {
		t.Error("implausible: want error")
	}
}

func TestPickBandeira(t *testing.T) {
	recs := []bandeiraRecord{
		{DatCompetencia: "2026-11-01", NomBandeiraAcionada: "Vermelha P1", VlrAdicionalBandeira: "44,63"},
		{DatCompetencia: "2026-10-01", NomBandeiraAcionada: "Verde", VlrAdicionalBandeira: ",00"},
		{DatCompetencia: "2026-09-01", NomBandeiraAcionada: "Amarela", VlrAdicionalBandeira: "18,85"},
	}
	got, err := pickBandeira(recs, day("2026-10-21"))
	if err != nil || got.Nome != "Verde" || got.Adicional != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Current month not announced yet: keep the latest earlier one.
	got, err = pickBandeira(recs[2:], day("2026-10-21"))
	if err != nil || got.Nome != "Amarela" || !near(got.Adicional, 0.01885) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestClientSendsFilters(t *testing.T) {
	var gotFilters map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/datastore_search" || r.URL.Query().Get("resource_id") != resourceTarifas {
			http.NotFound(w, r)
			return
		}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &gotFilters)
		w.Write([]byte(`{"success":true,"result":{"records":[{"DatInicioVigencia":"2026-08-22","DatFimVigencia":"2027-08-21","DscDetalhe":"Não se aplica","NomPostoTarifario":"Não se aplica","DscUnidadeTerciaria":"MWh","VlrTUSD":"414,04","VlrTE":"345,71"}]}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/", HTTPClient: srv.Client(), AllowHTTP: true}
	if _, err := c.Tarifa(context.Background(), DefaultFilter(), day("2026-10-21")); err != nil {
		t.Fatal(err)
	}
	if gotFilters["SigAgente"] != "CELESC" || gotFilters["DscBaseTarifaria"] != "Tarifa de Aplicação" {
		t.Fatalf("filters %v", gotFilters)
	}
}

func TestClientFailures(t *testing.T) {
	for name, body := range map[string]string{
		"success false": `{"success":false}`,
		"not json":      `<html>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
		c := &Client{BaseURL: srv.URL + "/", HTTPClient: srv.Client(), AllowHTTP: true}
		if _, err := c.Bandeira(context.Background(), day("2026-10-21")); err == nil {
			t.Errorf("%s: want error", name)
		}
		srv.Close()
	}
	// Without AllowHTTP a plain-http base URL is refused before any request.
	c := &Client{BaseURL: "http://example.invalid/", HTTPClient: http.DefaultClient}
	if _, err := c.Bandeira(context.Background(), day("2026-10-21")); err == nil {
		t.Error("http: want error")
	}
}
