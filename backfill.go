package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/aneel"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/backfill"
	"github.com/MateuxLucax/celesc-prometheus-exporter/internal/celesc"
)

// runBackfill implements `exporter backfill`: it reads the whole history once
// from ANEEL and Celesc, then writes every day of the period as OpenMetrics.
// It uses the same environment as the server, so ICMS and the tariff filter
// match what the live exporter reports.
func runBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	var (
		from     = fs.String("from", time.Now().AddDate(-1, 0, 0).Format(time.DateOnly), "first day (YYYY-MM-DD)")
		to       = fs.String("to", "", "stop before this instant (RFC 3339 or YYYY-MM-DD); default now. Set it to when the live exporter started scraping, so the two don't overlap")
		step     = fs.Duration("step", 2*time.Minute, fmt.Sprintf("sample spacing, at most %s (Prometheus' lookback)", backfill.MaxStep))
		out      = fs.String("o", "-", "output file, - for stdout")
		job      = fs.String("job", "energia", "job label, as in the Prometheus scrape config")
		instance = fs.String("instance", "celesc-prometheus-exporter:3000", "instance label, as in the Prometheus scrape config")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: celesc-prometheus-exporter backfill [flags]\n\nWrites OpenMetrics for: promtool tsdb create-blocks-from openmetrics FILE DIR")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg, filter, manual, _, err := loadConfig()
	if err != nil {
		return err
	}
	opt := backfill.Options{Step: *step, Labels: [][2]string{{"job", *job}, {"instance", *instance}}}
	if opt.From, err = time.ParseInLocation(time.DateOnly, *from, cfg.Location); err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	opt.To = time.Now()
	if *to != "" {
		if opt.To, err = time.Parse(time.RFC3339, *to); err != nil {
			if opt.To, err = time.ParseInLocation(time.DateOnly, *to, cfg.Location); err != nil {
				return errors.New("-to: want RFC 3339 or YYYY-MM-DD")
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ac := aneel.NewClient()
	in := backfill.Inputs{Config: cfg}
	if in.Tarifas, err = ac.Tarifas(ctx, filter); err != nil {
		return fmt.Errorf("ANEEL tariffs: %w", err)
	}
	// One row per month since 2015; 240 covers twenty years.
	if in.Bandeiras, err = ac.Bandeiras(ctx, 240); err != nil {
		return fmt.Errorf("ANEEL flags: %w", err)
	}
	if !manual {
		if in.CelescPage, err = celesc.NewClient().Page(ctx); err != nil {
			return fmt.Errorf("Celesc page: %w", err)
		}
	}

	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	rep, err := backfill.Write(w, in, opt)
	if err != nil {
		return err
	}
	log.Printf("backfill: %d days, %d skipped (inputs not published), %d samples", rep.Days, rep.SkippedDays, rep.Samples)
	return nil
}
