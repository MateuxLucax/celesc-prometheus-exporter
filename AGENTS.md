# AGENTS.md

Guidance for coding agents (Claude Code, Codex, Cursor, ...) working in this repository. `CLAUDE.md` only points here.

## What this is

A Prometheus exporter for the price of residential electricity in Brazil (defaults: Celesc, SC). It reads TE/TUSD and the bandeira from ANEEL's CKAN `datastore_search` API, scrapes monthly PIS/COFINS from Celesc's tariff page, takes ICMS/COSIP from env, and exports the components plus `energia_tarifa_final_reais_kwh`. This is a public repo: keep it standard-library only and treat every upstream byte as untrusted.

## Commands

The Go toolchain is pinned via `mise.toml`. If `go` isn't on PATH, prefix with `mise exec go@1.26.4 --`.

```bash
go test -race ./...           # all tests (fixtures + httptest fakes, no network)
LIVE_TESTS=1 go test -run TestLiveSources -v ./internal/exporter   # contract test against the real ANEEL/Celesc
go vet ./... && gofmt -l .    # vet + format check
go run .                      # run locally on :3000 against the real sources
go run . backfill -from 2025-10-21 -o energia.om   # past prices as OpenMetrics for promtool
```

## Architecture

- **`internal/fetch`** — the only network code. `Get` enforces HTTPS, the body cap and status checks; `NewClient` sets the timeout and the same-host redirect policy. `ParseBR` parses Brazilian-notation numbers and rejects NaN/Inf.
- **`internal/aneel`** — CKAN client. Resource IDs are constants; values arrive in R$/MWh and are converted to R$/kWh. `pickTarifa`/`pickBandeira` choose the row in force (falling back to the latest earlier one) and reject implausible values.
- **`internal/celesc`** — regex over the "Alíquotas de tributos" table rows (`MM/YYYY`, `x,xx%`, `y,yy%`); only digits are captured. Plausibility bounds are the nominal non-cumulative rates (1.65% / 7.6%).
- **`internal/exporter`** — refreshes all sources on an interval, keeps the last good value per source, and writes the text exposition format by hand (`writeSample` escapes labels). `Final` is the *por dentro* gross-up.
- **`internal/backfill`** — resolves inputs once per day over a period (via `TarifaHistory.At`, `BandeiraHistory.At`, `celesc.Parse`) and streams OpenMetrics series by series for `promtool tsdb create-blocks-from openmetrics`. Days whose month isn't published are skipped, never priced with another month's numbers. Step ≤ 4m so instant queries joining these series stay inside Prometheus' 5m lookback.
- **`main.go`** — env config (invalid values are fatal), the HTTP server, a `healthcheck` subcommand because the distroless image has no curl, and `backfill.go` for the `backfill` subcommand.

## Tests

- `internal/exporter/testdata` holds real ANEEL responses and the Celesc PIS/COFINS table captured on 2026-10-21. `golden_test.go` runs the real clients against them and pins R$/kWh for several dates (Oct/2026 = 0.93267, Aug/2026 after the reajuste = 1.02989). If a change moves those numbers, it changed the price: justify it, don't just update the expectation.
- `live_test.go` only runs with `LIVE_TESTS=1`; `.github/workflows/live.yml` runs it weekly to catch upstream format changes.
- `main_test.go` covers config parsing; invalid money-related config must stay a startup error.

## Deployment

Pushes to `main` build and publish `ghcr.io/mateuxlucax/celesc-prometheus-exporter` via `.github/workflows/ci.yml`, only after gofmt/vet/tests pass. Actions and base images are pinned (SHA / digest) and bumped by Dependabot.
