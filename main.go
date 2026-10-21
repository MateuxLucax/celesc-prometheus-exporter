// Command energia-tarifa-exporter exposes the price of residential electricity
// in Brazil as Prometheus metrics at /metrics: the tariff (TE + TUSD) and the
// bandeira from ANEEL's open data, the monthly PIS/COFINS from Celesc's tariff
// page, and ICMS from configuration.
//
// Run with "healthcheck" as the only argument to probe a running instance; the
// runtime image is distroless and has no curl.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; embed it for TZ

	"github.com/MateuxLucax/energia-tarifa-exporter/internal/aneel"
	"github.com/MateuxLucax/energia-tarifa-exporter/internal/celesc"
	"github.com/MateuxLucax/energia-tarifa-exporter/internal/exporter"
)

func main() {
	port := env("PORT", "3000")
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(port))
	}

	cfg, filter, pisCofinsManual, interval, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ac := aneel.NewClient()
	src := exporter.Sources{
		Tarifa: func(ctx context.Context, now time.Time) (aneel.Tarifa, error) {
			return ac.Tarifa(ctx, filter, now)
		},
		Bandeira: ac.Bandeira,
	}
	if !pisCofinsManual {
		src.Aliquotas = celesc.NewClient().Aliquotas
	}
	exp := exporter.New(src, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go exp.Start(ctx, interval)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", exp)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	pc := "celesc"
	if pisCofinsManual {
		pc = "manual"
	}
	log.Printf("energia-tarifa-exporter listening on :%s, %s %s %s/%s, ICMS %.2f%%, PIS/COFINS %s, refresh every %s",
		port, filter.Distribuidora, filter.Subgrupo, filter.Classe, filter.Subclasse, cfg.ICMSPercent, pc, interval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// loadConfig reads the environment. Money depends on these values, so anything
// set but invalid is a startup error rather than a silent default.
func loadConfig() (cfg exporter.Config, f aneel.Filter, pisCofinsManual bool, interval time.Duration, err error) {
	f = aneel.DefaultFilter()
	f.Distribuidora = env("DISTRIBUIDORA", f.Distribuidora)
	f.Subgrupo = env("SUBGRUPO", f.Subgrupo)
	f.Modalidade = env("MODALIDADE", f.Modalidade)
	f.Classe = env("CLASSE", f.Classe)
	f.Subclasse = env("SUBCLASSE", f.Subclasse)

	if interval, err = envDuration("REFRESH_INTERVAL", 6*time.Hour); err != nil {
		return
	}
	if interval < time.Minute {
		err = fmt.Errorf("REFRESH_INTERVAL must be at least 1m, got %s", interval)
		return
	}

	// 17% is Santa Catarina's residential rate above 150 kWh/month.
	if cfg.ICMSPercent, err = envPercent("ICMS_PERCENT", 17, 40); err != nil {
		return
	}
	// An empty value counts as unset, so "PIS_PERCENT=" in a .env file keeps
	// scraping instead of silently meaning 0%.
	pisSet := os.Getenv("PIS_PERCENT") != ""
	cofinsSet := os.Getenv("COFINS_PERCENT") != ""
	if pisSet != cofinsSet {
		err = fmt.Errorf("set both PIS_PERCENT and COFINS_PERCENT, or neither to read them from Celesc")
		return
	}
	pisCofinsManual = pisSet
	if pisCofinsManual {
		if cfg.ManualPIS, err = envPercent("PIS_PERCENT", 0, 1.65); err != nil {
			return
		}
		if cfg.ManualCOFINS, err = envPercent("COFINS_PERCENT", 0, 7.6); err != nil {
			return
		}
	} else if f.Distribuidora != "CELESC" {
		err = fmt.Errorf("PIS/COFINS are only scraped for CELESC; set PIS_PERCENT and COFINS_PERCENT for %s", f.Distribuidora)
		return
	}
	if cfg.COSIPReaisMes, err = envFloat("COSIP_REAIS_MES", 0, 0, 10000); err != nil {
		return
	}

	tz := env("TZ", "America/Sao_Paulo")
	if cfg.Location, err = time.LoadLocation(tz); err != nil {
		err = fmt.Errorf("TZ=%q: %w", tz, err)
	}
	return
}

func healthcheck(port string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/metrics")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", key, v, err)
	}
	return d, nil
}

func envPercent(key string, fallback, max float64) (float64, error) {
	return envFloat(key, fallback, 0, max)
}

// envFloat reads a decimal with a point ("17", "0.27"), within [min, max].
func envFloat(key string, fallback, min, max float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f != f || f < min || f > max {
		return 0, fmt.Errorf("%s=%q: want a number between %g and %g", key, v, min, max)
	}
	return f, nil
}
