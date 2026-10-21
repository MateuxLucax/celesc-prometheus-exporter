# Energia Tarifa Exporter

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/docker-%230db7ed.svg?style=for-the-badge&logo=docker&logoColor=white)
![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?style=for-the-badge&logo=Prometheus&logoColor=white)

A Prometheus exporter for the price of residential electricity in Brazil, with taxes, so a power meter's watts (a Tapo P110, a Shelly, a UPS) can be turned into R$ with the tariff that was actually in force at the time, instead of a number typed into a Grafana variable.

It combines:

| Part of the price | Source | Updated |
| --- | --- | --- |
| TE + TUSD (tarifa homologada) | [ANEEL open data](https://dadosabertos.aneel.gov.br/dataset/tarifas-distribuidoras-energia-eletrica) | yearly, at the distributor's reajuste |
| Bandeira tarifária | [ANEEL open data](https://dadosabertos.aneel.gov.br/dataset/bandeiras-tarifarias) | monthly |
| PIS / COFINS | [Celesc tariff page](https://www.celesc.com.br/tarifas-de-energia) (scraped), or `.env` | monthly |
| ICMS, COSIP | `.env` | rarely |

The defaults are a regular residential customer of **Celesc (Santa Catarina)**. Other distributors work through `DISTRIBUIDORA`/`SUBGRUPO`/... with PIS/COFINS set by hand, since only Celesc's page is scraped.

The final price uses the formula distributors print on the bill. Taxes are charged *por dentro*:

```
tarifa_final = (TE + TUSD + bandeira) / (1 − ICMS − PIS − COFINS)
```

## How to use

```bash
cp .env.example .env   # adjust ICMS_PERCENT and friends to match your bill
docker compose up -d
curl -s localhost:3000/metrics
```

To build from source: `docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build`, or `go run .` with Go 1.26.

### Configuration

All variables are optional; see [`.env.example`](.env.example). Invalid values stop the exporter at startup rather than silently falling back, because prices depend on them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DISTRIBUIDORA`, `SUBGRUPO`, `MODALIDADE`, `CLASSE`, `SUBCLASSE` | `CELESC`, `B1`, `Convencional`, `Residencial`, `Residencial` | Which ANEEL tariff row to use |
| `ICMS_PERCENT` | `17` | ICMS in percent (SC residential: 12 up to 150 kWh/month, 17 above) |
| `PIS_PERCENT`, `COFINS_PERCENT` | empty | Set both to pin them; leave both empty to scrape Celesc |
| `COSIP_REAIS_MES` | empty | Monthly street-lighting fee in R$ |
| `REFRESH_INTERVAL` | `6h` | How often to re-read the sources |
| `TZ` | `America/Sao_Paulo` | Which calendar month is "now" |
| `PORT` | `3000` | Listen port |

## Metrics

| Metric | Meaning |
| --- | --- |
| `energia_tarifa_final_reais_kwh` | Price per kWh with flag and taxes |
| `energia_tarifa_reais_kwh{componente="te\|tusd\|bandeira"}` | Components without taxes |
| `energia_imposto_ratio{imposto="icms\|pis\|cofins"}` | Tax rates as ratios |
| `energia_bandeira_info{bandeira="..."}` | The flag in force (always 1) |
| `energia_cosip_reais_mes` | COSIP, when configured |
| `energia_tarifa_vigencia_{inicio,fim}_timestamp_seconds`, `energia_bandeira_competencia_timestamp_seconds`, `energia_pis_cofins_referencia_timestamp_seconds` | Which period each input refers to |
| `energia_fonte_sucesso{fonte}`, `energia_fonte_ultimo_sucesso_timestamp_seconds{fonte}` | Health of each source |

A source that fails keeps its last good value and flips `energia_fonte_sucesso` to 0. Price metrics only appear once every input has been read at least once, so a cold start never exports a price built from zeros. If a month hasn't been published yet (flag, PIS/COFINS), the latest earlier one is used; the `*_timestamp_seconds` gauges show when that happens.

### Grafana / PromQL

With a Tapo P110 exporter (`tapo_power_watts`):

```promql
# R$ per hour, per device
avg_over_time(tapo_power_watts[1h]) / 1000
  * on() group_left energia_tarifa_final_reais_kwh

# R$ over the dashboard range, using the price of each moment
sum(sum_over_time((sum(tapo_power_watts) / 1000
  * on() group_left energia_tarifa_final_reais_kwh)[$__range:1m])) / 60
```

## Security

- Standard library only: no third-party Go modules.
- Distroless `nonroot` image, no shell; the compose file adds `read_only`, `cap_drop: ALL` and `no-new-privileges`, and binds to loopback. `/metrics` is unauthenticated and serves only public data, but keep it on a trusted network anyway.
- Outbound requests are HTTPS only, with a 30 s timeout, a 2 MiB body cap, and redirects followed only on the same host.
- Upstream data is validated before export: numbers must be finite and plausible, and the only upstream string that reaches the output (the flag name) is escaped and truncated.

## License

GPL-3.0, see [LICENSE](LICENSE).
