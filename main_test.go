package main

import (
	"strings"
	"testing"
	"time"
)

// configVars are every variable loadConfig reads; each test starts with all of
// them empty so the developer's own environment can't leak in.
var configVars = []string{
	"DISTRIBUIDORA", "SUBGRUPO", "MODALIDADE", "CLASSE", "SUBCLASSE",
	"REFRESH_INTERVAL", "ICMS_PERCENT", "PIS_PERCENT", "COFINS_PERCENT",
	"COSIP_REAIS_MES", "TZ",
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range configVars {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setEnv(t, nil)
	cfg, f, manual, interval, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if f.Distribuidora != "CELESC" || f.Subgrupo != "B1" || f.Subclasse != "Residencial" {
		t.Errorf("filter %+v", f)
	}
	if cfg.ICMSPercent != 17 || manual || interval != 6*time.Hour || cfg.COSIPReaisMes != 0 {
		t.Errorf("cfg %+v manual=%v interval=%s", cfg, manual, interval)
	}
	if cfg.Location.String() != "America/Sao_Paulo" {
		t.Errorf("location %s", cfg.Location)
	}
}

func TestLoadConfigManualPISCOFINS(t *testing.T) {
	setEnv(t, map[string]string{"PIS_PERCENT": "1.32", "COFINS_PERCENT": "6.08", "ICMS_PERCENT": "12", "COSIP_REAIS_MES": "25.40"})
	cfg, _, manual, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !manual || cfg.ManualPIS != 1.32 || cfg.ManualCOFINS != 6.08 || cfg.ICMSPercent != 12 || cfg.COSIPReaisMes != 25.4 {
		t.Errorf("cfg %+v manual=%v", cfg, manual)
	}
}

// Other distributors work, as long as PIS/COFINS are pinned (only Celesc's
// page is scraped).
func TestLoadConfigOtherDistribuidora(t *testing.T) {
	setEnv(t, map[string]string{"DISTRIBUIDORA": "COPEL-DIS", "PIS_PERCENT": "1", "COFINS_PERCENT": "4"})
	if _, f, _, _, err := loadConfig(); err != nil || f.Distribuidora != "COPEL-DIS" {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"only PIS":             {map[string]string{"PIS_PERCENT": "1"}, "set both"},
		"only COFINS":          {map[string]string{"COFINS_PERCENT": "4"}, "set both"},
		"other dist, scraping": {map[string]string{"DISTRIBUIDORA": "COPEL-DIS"}, "only scraped for CELESC"},
		"ICMS comma":           {map[string]string{"ICMS_PERCENT": "17,5"}, "ICMS_PERCENT"},
		"ICMS too high":        {map[string]string{"ICMS_PERCENT": "45"}, "ICMS_PERCENT"},
		"ICMS negative":        {map[string]string{"ICMS_PERCENT": "-1"}, "ICMS_PERCENT"},
		"ICMS NaN":             {map[string]string{"ICMS_PERCENT": "NaN"}, "ICMS_PERCENT"},
		"PIS above nominal":    {map[string]string{"PIS_PERCENT": "2", "COFINS_PERCENT": "4"}, "PIS_PERCENT"},
		"COFINS above nominal": {map[string]string{"PIS_PERCENT": "1", "COFINS_PERCENT": "8"}, "COFINS_PERCENT"},
		"COSIP negative":       {map[string]string{"COSIP_REAIS_MES": "-5"}, "COSIP_REAIS_MES"},
		"interval garbage":     {map[string]string{"REFRESH_INTERVAL": "daily"}, "REFRESH_INTERVAL"},
		"interval too short":   {map[string]string{"REFRESH_INTERVAL": "5s"}, "at least 1m"},
		"bad TZ":               {map[string]string{"TZ": "Mars/Olympus"}, "TZ"},
	} {
		t.Run(name, func(t *testing.T) {
			setEnv(t, tc.env)
			_, _, _, _, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
