package celesc

import (
	"testing"
	"time"
)

// page mirrors the markup of the "Alíquotas de tributos" table on the Celesc
// tariff page, plus an unrelated table that must not match.
const page = `
<table class="table table-striped" style="width: 100%;"><tbody>
<tr><td colspan="3"><h4><strong>Alíquotas de tributos</strong></h4></td></tr>
<tr><td><strong>Referência</strong></td><td><strong>PIS</strong></td><td><strong>COFINS</strong></td></tr>
<tr>
<td>10/2026</td>
<td>0,27%</td>
<td>1,27%</td>
</tr>
<tr>
<td>09/2026</td>
<td>0,89%</td>
<td>4,12%</td>
</tr>
</tbody></table>
<table><tr><td>ICMS município de Rio Negro (PR)</td><td>29%</td><td>18%</td></tr></table>
`

func TestParseCurrentMonth(t *testing.T) {
	got, err := Parse([]byte(page), time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.PIS != 0.27 || got.COFINS != 1.27 || got.Referencia.Month() != time.October {
		t.Fatalf("got %+v", got)
	}
}

func TestParseFallsBackToLatestEarlierMonth(t *testing.T) {
	// November not published yet: October's rates are still the best guess.
	got, err := Parse([]byte(page), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || got.Referencia.Month() != time.October {
		t.Fatalf("got %+v, %v", got, err)
	}
	// September asks for September, not the later October row.
	got, err = Parse([]byte(page), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil || got.PIS != 0.89 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseRejects(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if _, err := Parse([]byte("<html>maintenance</html>"), now); err == nil {
		t.Error("missing table: want error")
	}
	bad := `<td>10/2026</td><td>16,5%</td><td>1,27%</td>`
	if _, err := Parse([]byte(bad), now); err == nil {
		t.Error("implausible PIS: want error")
	}
	month13 := `<td>13/2026</td><td>0,27%</td><td>1,27%</td>`
	if _, err := Parse([]byte(month13), now); err == nil {
		t.Error("month 13: want error")
	}
}

func TestParseToleratesNbsp(t *testing.T) {
	// The real page has rows like this one (01/2025, 05/2020).
	row := `<td>01/2025</td><td>0,07%&nbsp;</td><td>&nbsp;0,33 %&nbsp;</td>`
	got, err := Parse([]byte(row), time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.PIS != 0.07 || got.COFINS != 0.33 || got.Referencia.Month() != time.January {
		t.Fatalf("got %+v", got)
	}
}
