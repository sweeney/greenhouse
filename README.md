# Greenhouse

Read-side **climate / environment reporting** service for the swee.net home. It
turns the per-sensor environmental telemetry statehouse writes to InfluxDB
(`device_environment`: temperature, humidity, pressure, wind, rainfall, light,
UV) into **time-series charts** over arbitrary windows, for indoor sensors and
the outdoor weather station.

It is the climate sibling of [`countinghouse`](../countinghouse) (energy cost)
and `statehouse` (real-time state), and mirrors their conventions. See
`PLAN.md` for the full design and `CLAUDE.md` for the invariants.

## Core ideas

- **Read-side only.** No MQTT, no ingest. Query Influx, shape for charts.
- **Non-additive.** Climate values are bucketed and grouped with
  **mean / min / max / last** — never summed. `group_by=location` is the **mean**
  across a room's sensors, not a total.
- **Left-edge buckets.** Flux `aggregateWindow` is called with
  `timeSrc: "_start"` so values align to the Go-owned canonical bucket axis;
  empty buckets are `null` (no reading), not zero.

## HTTP API

Listen port **:8082** (statehouse :8080, countinghouse :8081). All times
Europe/London. `/healthz` and `/openapi.json` are public; everything else
requires a Bearer JWT (user **or** service token).

| Route | Returns |
|---|---|
| `GET /healthz` | status, version, uptime, influx_reachable, remote_config status |
| `GET /openapi.json` | the OpenAPI spec as JSON |
| `GET /devices` | climate device catalog (class `environmental_sensor`): id, display_name, location, class, and a `fields` hint |
| `GET /devices/{id}/series` | single-device, single-field time-series |
| `GET /series` | multi-series; `group_by` device (default) or location (mean per room) |
| `GET /devices/{id}/latest` | the device's most recent reading across its fields (within the last 7 days) |
| `GET /fields` | the field registry (name, unit, default fn) |

### Series parameters

- `window` — `today` (default) \| `week` \| `month` \| `custom`. `from`/`to`
  (RFC3339) are valid **only** with `custom` — required there, and a 400 for any
  other window (they are not silently ignored). A `custom` span over ~2 years
  (Influx retention) → 400.
- `interval` — `5m,15m,30m,1h,6h,1d`; smart default per window, ~1000-bucket cap.
- `field` — one of `temperature_c, humidity_pct, pressure_hpa, wind_speed_ms,
  wind_dir_deg, rainfall_mm, illuminance_lux, uv_index`. Default `temperature_c`.
  Unknown field → 400.
- `fn` — `mean` (default) \| `min` \| `max` \| `last`. `sum` is deliberately not
  offered (non-additive). Bad fn → 400.
- `group_by` — `device` (default) \| `location`. Bad value → 400.
- `devices` — (`/series` only) CSV of device ids to chart, e.g.
  `devices=climate_groundfloor,climate_firstfloor`. Restricts the series to those
  sensors (omit for all climate devices). An unknown or non-climate id → 400.
- `locations` — (`/series` only) CSV of location tags to chart, e.g.
  `locations=ground_floor,first_floor`. The candidate set is always climate
  sensors only, so a non-climate device sharing a location is never included, and
  a location with no climate sensor → 400. Composes with `devices` as AND.
- `shape` — `columns` (default, shared buckets axis + per-series arrays) \|
  `rows` (flat one-row-per-(series,bucket)). Both carry `field`/`unit`/`fn`.

The `fields` hint on `/devices` comes from the device config's explicit `fields`
list when present in `statehouse_devices`, otherwise it falls back to the full
field registry (greenhouse can't know per-device coverage without querying
Influx; the registry is a safe superset for building a picker).

## Config

Local YAML (`/etc/greenhouse/config.yaml`, see `config/config.example.yaml`):
`http.listen`, `influx{url,org,bucket,token_file}`,
`identity{base_url,client_id,client_secret}`, `remote_config.base_url`,
`house.timezone`. Device inventory is fetched from the remote config service
(`statehouse_devices` namespace only — **no** tariffs). Fetches are fail-open
(log + keep last-known) with SIGHUP reload. Greenhouse needs its own Influx
**read token** (statehouse bucket) and `client_id`/`secret` in id.swee.net.

## Development

```
make build      # go build -o bin/greenhouse ./cmd/greenhouse
make test       # go test -race -count=1 ./...
make lint       # go vet ./...
make lint-spec  # spectral lint of the OpenAPI doc
make fmt        # gofmt -w .
```

CI (`.github/workflows/ci.yml`) runs build, vet, test, gofmt, staticcheck and
spectral on `main`.

## Deploy

`make deploy` (= `./deploy/deploy.sh sweeney@garibaldi`) builds linux/amd64,
uploads, symlinks, and restarts the systemd unit on garibaldi. First-time setup
is `deploy/install.sh` (run with sudo on the host) + `deploy/sudoers.sh`.

## Follow-up

The generic time-series scaffolding (windowing, interval/bucket axis,
columnar/rows shapes, Influx Querier+fake, Server/auth/CORS/spec skeleton,
config Fetcher) is **copied** from countinghouse. The plan is to extract the
common core into a shared `github.com/sweeney/timeseries` module once greenhouse
is real (PLAN.md §10).
