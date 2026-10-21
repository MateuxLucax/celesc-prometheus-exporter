# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Prometheus exporter for the price of residential electricity in Brazil (defaults: Celesc, SC). It reads TE/TUSD and the bandeira from ANEEL's CKAN `datastore_search` API, scrapes monthly PIS/COFINS from Celesc's tariff page, takes ICMS/COSIP from env, and exports the components plus `energia_tarifa_final_reais_kwh`. This is a public repo: keep it standard-library only and treat every upstream byte as untrusted.

## Commands

The Go toolchain is pinned via `mise.toml`. If `go` isn't on PATH, prefix with `mise exec go@1.26.4 --`.

```bash
go test -race ./...           # all tests (httptest fakes, no real network)
go vet ./... && gofmt -l .    # vet + format check
go run .                      # run locally on :3000 against the real sources
```

## Architecture

- **`internal/fetch`** — the only network code. `Get` enforces HTTPS, the body cap and status checks; `NewClient` sets the timeout and the same-host redirect policy. `ParseBR` parses Brazilian-notation numbers and rejects NaN/Inf.
- **`internal/aneel`** — CKAN client. Resource IDs are constants; values arrive in R$/MWh and are converted to R$/kWh. `pickTarifa`/`pickBandeira` choose the row in force (falling back to the latest earlier one) and reject implausible values.
- **`internal/celesc`** — regex over the "Alíquotas de tributos" table rows (`MM/YYYY`, `x,xx%`, `y,yy%`); only digits are captured. Plausibility bounds are the nominal non-cumulative rates (1.65% / 7.6%).
- **`internal/exporter`** — refreshes all sources on an interval, keeps the last good value per source, and writes the text exposition format by hand (`writeSample` escapes labels). `Final` is the *por dentro* gross-up.
- **`main.go`** — env config (invalid values are fatal), the HTTP server, and a `healthcheck` subcommand because the distroless image has no curl.

## Deployment

Pushes to `main` build and publish `ghcr.io/mateuxlucax/energia-tarifa-exporter` via `.github/workflows/ci.yml`, only after gofmt/vet/tests pass. Actions and base images are pinned (SHA / digest) and bumped by Dependabot.
