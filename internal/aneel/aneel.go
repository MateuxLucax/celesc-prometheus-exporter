// Package aneel reads tariffs and the bandeira tarifária from ANEEL's open-data
// portal (dadosabertos.aneel.gov.br), a CKAN instance whose datastore_search
// action returns JSON rows.
//
// All values ANEEL publishes here are in R$/MWh and exclude taxes (ICMS, PIS,
// COFINS); this package converts them to R$/kWh and leaves taxes to the caller.
package aneel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/MateuxLucax/energia-tarifa-exporter/internal/fetch"
)

// DefaultBaseURL is ANEEL's CKAN API root.
const DefaultBaseURL = "https://dadosabertos.aneel.gov.br/api/3/action/"

// CKAN resource IDs of the datasets this package reads. They are stable across
// dataset refreshes (ANEEL regenerates the rows, not the resources).
const (
	// "tarifas-distribuidoras-energia-eletrica": tarifas homologadas.
	resourceTarifas = "fcf2906c-7c32-4b9b-a637-054e7a5234f4"
	// "bandeiras-tarifarias": Bandeira Tarifária - Acionamento.
	resourceBandeiraAcionamento = "0591b8f6-fe54-437b-b72b-1aa2efd46e42"
)

// Filter selects the tariff row of one consumer class at one distributor. The
// zero value is not useful; start from DefaultFilter.
type Filter struct {
	Distribuidora string // SigAgente, e.g. "CELESC"
	Subgrupo      string // DscSubGrupo, e.g. "B1"
	Modalidade    string // DscModalidadeTarifaria, e.g. "Convencional"
	Classe        string // DscClasse, e.g. "Residencial"
	Subclasse     string // DscSubClasse, e.g. "Residencial"
}

// DefaultFilter is a regular (non-low-income) residential customer at Celesc.
func DefaultFilter() Filter {
	return Filter{
		Distribuidora: "CELESC",
		Subgrupo:      "B1",
		Modalidade:    "Convencional",
		Classe:        "Residencial",
		Subclasse:     "Residencial",
	}
}

// Tarifa is the homologated tariff in force, in R$/kWh, without taxes.
type Tarifa struct {
	TE             float64
	TUSD           float64
	InicioVigencia time.Time
	FimVigencia    time.Time
	Resolucao      string
}

// Bandeira is the tariff flag of a month and its surcharge in R$/kWh, without
// taxes.
type Bandeira struct {
	Nome        string
	Adicional   float64
	Competencia time.Time
}

// Client queries ANEEL's datastore.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	// AllowHTTP permits a plain-http BaseURL; only tests set it.
	AllowHTTP bool
}

// NewClient returns a Client for the real ANEEL API.
func NewClient() *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTPClient: fetch.NewClient()}
}

type searchResponse[T any] struct {
	Success bool `json:"success"`
	Result  struct {
		Records []T `json:"records"`
	} `json:"result"`
}

func search[T any](ctx context.Context, c *Client, params url.Values) ([]T, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, err
	}
	u = u.JoinPath("datastore_search")
	u.RawQuery = params.Encode()

	body, err := fetch.Get(ctx, c.HTTPClient, u, c.AllowHTTP)
	if err != nil {
		return nil, err
	}
	var resp searchResponse[T]
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding ANEEL response: %w", err)
	}
	if !resp.Success {
		return nil, errors.New("ANEEL datastore_search returned success=false")
	}
	return resp.Result.Records, nil
}

type tarifaRecord struct {
	DscREH            string `json:"DscREH"`
	DatInicioVigencia string `json:"DatInicioVigencia"`
	DatFimVigencia    string `json:"DatFimVigencia"`
	DscDetalhe        string `json:"DscDetalhe"`
	NomPostoTarifario string `json:"NomPostoTarifario"`
	Unidade           string `json:"DscUnidadeTerciaria"`
	VlrTUSD           string `json:"VlrTUSD"`
	VlrTE             string `json:"VlrTE"`
}

// Tarifa returns the tariff in force on day now for the filtered class.
func (c *Client) Tarifa(ctx context.Context, f Filter, now time.Time) (Tarifa, error) {
	filters, err := json.Marshal(map[string]string{
		"SigAgente":              f.Distribuidora,
		"DscSubGrupo":            f.Subgrupo,
		"DscModalidadeTarifaria": f.Modalidade,
		"DscClasse":              f.Classe,
		"DscSubClasse":           f.Subclasse,
		"DscBaseTarifaria":       "Tarifa de Aplicação",
	})
	if err != nil {
		return Tarifa{}, err
	}
	recs, err := search[tarifaRecord](ctx, c, url.Values{
		"resource_id": {resourceTarifas},
		"filters":     {string(filters)},
		"sort":        {"DatInicioVigencia desc"},
		"limit":       {"50"},
	})
	if err != nil {
		return Tarifa{}, err
	}
	return pickTarifa(recs, now)
}

// pickTarifa chooses the row in force on now: the one whose vigência contains
// it, or failing that the most recent one that already started. Rows with a
// "detalhe" (SCEE, APE, ...) or a time-of-use posto are other products.
func pickTarifa(recs []tarifaRecord, now time.Time) (Tarifa, error) {
	day := dateOf(now)
	var best *Tarifa
	for _, r := range recs {
		if r.DscDetalhe != "Não se aplica" || r.NomPostoTarifario != "Não se aplica" {
			continue
		}
		if r.Unidade != "MWh" {
			continue
		}
		inicio, err1 := time.Parse(time.DateOnly, r.DatInicioVigencia)
		fim, err2 := time.Parse(time.DateOnly, r.DatFimVigencia)
		if err1 != nil || err2 != nil || inicio.After(day) {
			continue
		}
		te, err1 := fetch.ParseBR(r.VlrTE)
		tusd, err2 := fetch.ParseBR(r.VlrTUSD)
		if err1 != nil || err2 != nil {
			continue
		}
		t := Tarifa{TE: te / 1000, TUSD: tusd / 1000, InicioVigencia: inicio, FimVigencia: fim, Resolucao: r.DscREH}
		if !fim.Before(day) {
			return t, validTarifa(t)
		}
		if best == nil || t.InicioVigencia.After(best.InicioVigencia) {
			best = &t
		}
	}
	if best == nil {
		return Tarifa{}, errors.New("no ANEEL tariff row matches the filter")
	}
	return *best, validTarifa(*best)
}

// validTarifa rejects values no residential tariff could have, so a format
// change upstream shows up as a failed refresh instead of a wrong price.
func validTarifa(t Tarifa) error {
	if t.TE < 0 || t.TUSD <= 0 || t.TE+t.TUSD > 5 {
		return fmt.Errorf("implausible tariff TE=%.5f TUSD=%.5f R$/kWh", t.TE, t.TUSD)
	}
	return nil
}

type bandeiraRecord struct {
	DatCompetencia       string `json:"DatCompetencia"`
	NomBandeiraAcionada  string `json:"NomBandeiraAcionada"`
	VlrAdicionalBandeira string `json:"VlrAdicionalBandeira"`
}

// Bandeira returns the flag of now's month, or the latest published one if
// ANEEL hasn't announced the current month yet.
func (c *Client) Bandeira(ctx context.Context, now time.Time) (Bandeira, error) {
	recs, err := search[bandeiraRecord](ctx, c, url.Values{
		"resource_id": {resourceBandeiraAcionamento},
		"sort":        {"DatCompetencia desc"},
		"limit":       {"6"},
	})
	if err != nil {
		return Bandeira{}, err
	}
	return pickBandeira(recs, now)
}

func pickBandeira(recs []bandeiraRecord, now time.Time) (Bandeira, error) {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var best *Bandeira
	for _, r := range recs {
		comp, err := time.Parse(time.DateOnly, r.DatCompetencia)
		if err != nil || comp.After(month) {
			continue
		}
		v, err := fetch.ParseBR(r.VlrAdicionalBandeira)
		if err != nil || v < 0 || v > 1000 {
			continue
		}
		b := Bandeira{Nome: r.NomBandeiraAcionada, Adicional: v / 1000, Competencia: comp}
		if best == nil || b.Competencia.After(best.Competencia) {
			best = &b
		}
	}
	if best == nil {
		return Bandeira{}, errors.New("no ANEEL bandeira row for this or an earlier month")
	}
	return *best, nil
}

// dateOf drops the clock, keeping the calendar day of t in t's location.
func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
