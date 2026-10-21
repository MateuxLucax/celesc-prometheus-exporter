package fetch

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseBR parses a number in Brazilian notation as ANEEL and Celesc publish
// it: "345,71", "1.234,56", ",00" or "0,27%". Thousands dots are dropped and
// the decimal comma becomes a point.
func ParseBR(s string) (float64, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimSuffix(t, "%")
	t = strings.TrimSpace(t)
	t = strings.ReplaceAll(t, ".", "")
	t = strings.Replace(t, ",", ".", 1)
	if strings.HasPrefix(t, ".") {
		t = "0" + t
	}
	v, err := strconv.ParseFloat(t, 64)
	// ParseFloat accepts "NaN" and "Inf", which no tariff should ever be.
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return v, nil
}
