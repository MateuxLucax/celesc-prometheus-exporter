// Package celesc reads the monthly PIS/COFINS rates Celesc publishes on its
// tariff page. Distributors pass these federal taxes through at an effective
// rate that changes every month, and Celesc only publishes them as an HTML
// table, so this package scrapes that one table.
package celesc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/MateuxLucax/energia-tarifa-exporter/internal/fetch"
)

// DefaultURL is the Celesc page holding the "Alíquotas de tributos" table.
const DefaultURL = "https://www.celesc.com.br/tarifas-de-energia"

// Aliquotas are the PIS and COFINS rates of one reference month, in percent.
type Aliquotas struct {
	PIS        float64
	COFINS     float64
	Referencia time.Time // first day of the month the rates apply to
}

// Client fetches and parses the Celesc page.
type Client struct {
	URL        string
	HTTPClient *http.Client
	// AllowHTTP permits a plain-http URL; only tests set it.
	AllowHTTP bool
}

// NewClient returns a Client for the real Celesc page.
func NewClient() *Client {
	return &Client{URL: DefaultURL, HTTPClient: fetch.NewClient()}
}

// Aliquotas returns the rates of now's month, or of the latest month published
// before it.
func (c *Client) Aliquotas(ctx context.Context, now time.Time) (Aliquotas, error) {
	body, err := c.Page(ctx)
	if err != nil {
		return Aliquotas{}, err
	}
	return Parse(body, now)
}

// Page fetches the raw tariff page, for callers that Parse it for many dates.
func (c *Client) Page(ctx context.Context) ([]byte, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	return fetch.Get(ctx, c.HTTPClient, u, c.AllowHTTP)
}

// rowRe matches one table row: <td>MM/YYYY</td><td>x,xx%</td><td>y,yy%</td>,
// tolerating attributes and whitespace. It only ever captures digits, commas
// and dots, so nothing from the page reaches the metrics except numbers.
var rowRe = regexp.MustCompile(`(?s)<td[^>]*>\s*(\d{2})/(\d{4})\s*</td>\s*<td[^>]*>\s*([\d.,]+)\s*%\s*</td>\s*<td[^>]*>\s*([\d.,]+)\s*%\s*</td>`)

// Parse extracts the rates for now's month (or the latest earlier one) from the
// page HTML.
func Parse(page []byte, now time.Time) (Aliquotas, error) {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var best *Aliquotas
	for _, m := range rowRe.FindAllSubmatch(page, -1) {
		mm, _ := strconv.Atoi(string(m[1]))
		yyyy, _ := strconv.Atoi(string(m[2]))
		if mm < 1 || mm > 12 {
			continue
		}
		ref := time.Date(yyyy, time.Month(mm), 1, 0, 0, 0, 0, time.UTC)
		if ref.After(month) {
			continue
		}
		pis, err1 := fetch.ParseBR(string(m[3]))
		cofins, err2 := fetch.ParseBR(string(m[4]))
		if err1 != nil || err2 != nil {
			continue
		}
		a := Aliquotas{PIS: pis, COFINS: cofins, Referencia: ref}
		if best == nil || a.Referencia.After(best.Referencia) {
			best = &a
		}
	}
	if best == nil {
		return Aliquotas{}, errors.New("PIS/COFINS table not found on the Celesc page")
	}
	// The nominal non-cumulative rates are 1.65% and 7.6%; the effective rate
	// is lower after credits. Anything outside that means the page changed.
	if best.PIS < 0 || best.PIS > 1.65 || best.COFINS < 0 || best.COFINS > 7.6 {
		return Aliquotas{}, fmt.Errorf("implausible PIS=%.2f%% COFINS=%.2f%% for %s", best.PIS, best.COFINS, best.Referencia.Format("01/2006"))
	}
	return *best, nil
}
