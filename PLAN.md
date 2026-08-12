# Greenhouse — Design & Implementation Plan

Greenhouse is a **read-side climate / environment reporting service** for the swee.net home.
It turns the per-sensor environmental telemetry that **statehouse** writes to InfluxDB into
**time-series charts of temperature, humidity, pressure, wind, rainfall, light and UV** over
arbitrary windows — for indoor sensors and the outdoor weather station. It is the climate
sibling of **countinghouse** (energy cost) and **statehouse** (real-time state).

> Mirrors countinghouse's conventions (which mirror statehouse's). It is **not** an energy
> service: there is no cost, tariff, bill, or counter/integral. Climate data is **non-additive**.

## 1. Invariants

- **Read-side only.** No MQTT, no ingest, no real-time state. Query Influx, shape for charts.
- **Stateless w.r.t. accumulation.** Derive answers on query; survive restart with zero loss.
- **Non-additive aggregation.** You never *sum* temperatures. Bucket/group with **mean / min /
  max / last** — the core difference from countinghouse's additive kWh.

## 2. Module & dependencies

`go.mod`: module `github.com/sweeney/greenhouse`, **go 1.25.0**. Pin:
- `github.com/sweeney/identity/common v0.3.0` — `auth.JWKSVerifier` (inbound), **`auth.TokenSource`**
  (outbound client_credentials — newly shared in v0.3.0), `spec.Converter` (OpenAPI).
- `github.com/influxdata/influxdb-client-go/v2 v2.14.0` — query side.
- `gopkg.in/yaml.v3 v3.0.1`.

## 3. Data in Influx (verified live)

Measurement **`device_environment`**, tags `device_id`, `class`, `location`. Fields (with units):

| Field | Unit | Notes |
|---|---|---|
| `temperature_c` | °C | indoor + outdoor |
| `humidity_pct` | % | indoor + outdoor |
| `pressure_hpa` | hPa | weather station |
| `wind_speed_ms` | m/s | weather station |
| `wind_dir_deg` | ° | weather station |
| `rainfall_mm` | mm | weather station |
| `illuminance_lux` | lux | weather station |
| `uv_index` | index | weather station |

Devices (classes `environmental_sensor` + `fire_alarm` in `statehouse_devices`): `climate_basement`,
`climate_groundfloor`, `climate_firstfloor`, `climate_secondfloor`, `climate_thirdfloor`
(indoor temp/humidity), `glowsensorth1` (network cabinet), and `climate_weatherstation`
(outdoor, full set; `location: garden`). Bucket: `statehouse` (org `swee.net`, 2-yr retention).

### Query (the heart of greenhouse)
One field, mean/min/max per bucket, DST-aware local buckets, **left-edge stamped** (the
countinghouse bucket-alignment lesson — `timeSrc:"_start"` is mandatory):
```flux
import "timezone"
from(bucket: "statehouse")
  |> range(start: windowStart, stop: windowStop)
  |> filter(fn: (r) => r._measurement == "device_environment" and r._field == "temperature_c")
  |> filter(fn: (r) => contains(value: r.device_id, set: [...]))
  |> aggregateWindow(every: 1h, fn: mean, timeSrc: "_start",
                     location: timezone.location(name: "Europe/London"), createEmpty: true)
```
`createEmpty:true` + the Go-owned canonical bucket axis + demux (copied from countinghouse).
No counter/integral/reset handling — climate fields are plain gauge readings.

## 4. Package layout (mirror countinghouse, energy bits dropped)

```
cmd/greenhouse/main.go
internal/config/      config.go (Load; http/influx/identity/remote_config/house), device.go
                      (DeviceConfig + normalise, shared statehouse_devices shape), remote.go
                      (Fetcher — statehouse_devices ONLY; no energy_tariffs)
internal/influx/      client.go, query.go (Querier/Row/FakeQuerier), series_query.go
                      (BuildFieldSeriesFlux — field+fn parameterised, timeSrc:"_start")
internal/climate/     fields.go (field registry: name→unit, allowed fns), window.go +
                      interval.go (COPIED from countinghouse), series.go (bucket axis, demux,
                      AssembleSeries with MEAN/MIN/MAX group aggregation, SeriesResponse),
                      series_rows.go (columnar↔rows reshape + types)
internal/httpapi/     server.go, auth.go (dual user+service token), cors.go, spec.go,
                      handlers.go, openapi.yaml, spec_test.go
internal/testutil/    clock.go, ptrs.go (copied)
deploy/  Makefile  config/config.example.yaml  .github/workflows/ci.yml  .spectral.yml
```

## 5. Field registry & aggregation

`internal/climate/fields.go` — the field metadata that makes responses self-describing:
```go
type Field struct { Name, Unit string; DefaultFn string } // DefaultFn: "mean"
```
e.g. `temperature_c`→{°C, mean}, `humidity_pct`→{%, mean}, `rainfall_mm`→{mm, **sum**? no —
keep mean per bucket for v1; rainfall is genuinely additive but treat uniformly as a gauge for
the scaffold and revisit}, etc. Allowed `fn`: `mean` (default), `min`, `max`, `last`.

**Aggregation across group members** (e.g. a room with >1 sensor, or `group_by=room`):
the group value per bucket is the **mean of the member readings** — NOT a sum. This is the
defining difference from countinghouse's `AssembleSeries`.

## 6. HTTP API

Plain `net/http.ServeMux`; mirror countinghouse's Server/auth/CORS/spec/writeJSON/healthz.
All times Europe/London; CORS permissive (browser consumers). Auth: `/healthz` + `/openapi.json`
public; everything else requires a Bearer JWT (user **or** service token, via `common/auth`).

| Route | Auth | Returns |
|---|---|---|
| `GET /healthz` | public | status, version, uptime, influx_reachable, remote_config status |
| `GET /openapi.json` | public | spec (path-coverage test enforced) |
| `GET /devices` | yes | climate device catalog: id, display_name, room, class, `environment_fields` (which it reports) |
| `GET /devices/{id}/series?window=&interval=&field=&fn=&shape=` | yes | single-device, single-field time-series (columnar or rows), with `unit` |
| `GET /series?window=&interval=&field=&fn=&group_by=&shape=` | yes | multi-series; `group_by`: `device` (default), `room` (mean per room) |
| `GET /devices/{id}/latest` | yes | the device's most recent reading across all its fields (for dashboards) |
| `GET /fields` | yes | the field registry (name, unit, default fn) so consumers can build pickers |

**Windows/intervals/shapes:** identical to countinghouse — `today|week|month|custom`,
`5m..1d` with smart default + ~1000-bucket cap, `shape=columns|rows`. Reuse those modules.
Series response carries `field` + `unit` + `fn`. **No** `/bill`, `/tariffs`, `/cost`, `/events`.

## 7. Config

Local YAML (`/etc/greenhouse/config.yaml`) — same shape as countinghouse minus tariffs:
`http.listen: ":8082"` (statehouse :8080, countinghouse :8081), `influx{url,org,bucket,token_file}`,
`identity{base_url,client_id,client_secret}`, `remote_config.base_url`, `house.timezone`.
Remote config: fetch **one devices namespace only** — `site.devices_namespace`, required with no
default since the shared `statehouse_devices` document was deleted (no `energy_tariffs`);
fail-open, SIGHUP reload.
Greenhouse needs its own Influx **read token** (statehouse bucket) and `client_id/secret` in id.swee.net.

## 8. Testing (mirror countinghouse density)

`FakeQuerier` returning bucketed rows; `FakeClock`; table/fixture tests for windowing (BST/DST),
interval cap, bucket-axis alignment (assert builders set `timeSrc:"_start"`), mean/min/max group
aggregation, columnar↔rows reshape, field/unit metadata, auth (user + service), path-coverage.
`make test` = `go test -race -count=1 ./...`. CI mirrors countinghouse (build/vet/test/gofmt/
staticcheck/spectral). House rule: keep openapi.yaml + README in sync with endpoints.

## 9. Milestones

1. **Scaffold** — go.mod, Makefile, testutil, config.Load + structs, config.example.yaml.
2. **Influx + field series builder** — Querier/Client/FakeQuerier (copy), `BuildFieldSeriesFlux`
   (field+fn parameterised, timeSrc:"_start") — tested.
3. **Windowing + interval** — copied from countinghouse, tested (DST).
4. **Climate series core** — fields registry, bucket axis/demux, `AssembleSeries` (mean/min/max,
   non-additive, group_by device|room), columnar+rows, units — tested.
5. **HTTP server** — Server/healthz/openapi/path-coverage + dual-token auth + CORS.
6. **Handlers** — /devices, /series, /devices/{id}/series, /devices/{id}/latest, /fields.
7. **Fetcher (statehouse_devices) + main.go** — SIGHUP, graceful shutdown.
8. **Deploy** (garibaldi, systemd, :8082) + CI + README + demo (reuse countinghouse's chart demos
   pointed at climate fields).

## 10. Shared-library follow-up (deferred — see countinghouse PLAN / memory)

Greenhouse deliberately **copies** countinghouse's domain-agnostic scaffolding (windowing,
interval/bucket axis, columnar/rows shapes, the Influx Querier+fake, the Server/auth/CORS/spec
skeleton, config Fetcher). Once greenhouse is real, **extract that common core into a shared
`github.com/sweeney/timeseries` module** consumed by both services — factored from two real
implementations, not one. The subtle, correctness-critical bits (window/DST, bucket alignment
`timeSrc:"_start"`) are the priority to de-duplicate, since copy-drift there is dangerous.
`auth.TokenSource` already moved to `identity/common` v0.3.0 as the first step.

---

# 11. Current state & operational intel (handoff)

**Status (2026-06-12):** scaffold built and **green** — `gofmt`/`go vet`/`go build`/
`go test -race ./...` (packages: climate, config, httpapi, influx, testutil), `staticcheck`
clean, `spectral` 0 errors, binary builds. Milestones 1–7 done + deploy scripts written.
Initial commit on `main`; remote `git@github.com:sweeney/greenhouse.git` exists (not pushed yet
— pushing triggers CI, which is green).

**Deferred / next milestones:**
- **Demo pages** — port countinghouse's `demo/` chart apps (`index.html`, `breakdown.html`,
  `overlay.html`) pointed at climate fields. They use a gitignored `token.local.js`
  (`window.CH_DEV_TOKEN`) + paste-into-Connection-settings; mirror that. CORS is already wired.
- **Deploy** — scripts exist (`deploy/`, port :8082); not run. Needs the two secrets below first.
- **Rainfall** — `rainfall_mm` is treated as a uniform gauge (mean) in v1; revisit if true
  per-bucket rainfall *totals* are wanted (that field IS additive, unlike temperature).
- **Shared `timeseries` module** — the big follow-up (§10): extract the copied window/bucket/series
  scaffolding from countinghouse + greenhouse once both are real.

**Provisioning still required before deploy:**
1. A greenhouse **Influx read token** scoped to the `statehouse` bucket. Mint on garibaldi:
   `docker exec influxdb influx auth create --org swee.net --read-bucket f92ece14ec7e190f`
   → place at `/etc/greenhouse/influx-token` (prod) or a local file for dev.
2. A greenhouse **`client_id`/`client_secret`** registered in id.swee.net (for the outbound
   `auth.TokenSource` that fetches `statehouse_devices`).

**InfluxDB access (the data source):** Docker container `influxdb` on **garibaldi**
(`192.168.1.200:8086`, InfluxDB OSS v2.8.0), org `swee.net`, bucket **`statehouse`** (2-yr
retention; statehouse writes it). To explore from the LAN, either:
- `ssh sweeney@garibaldi 'docker exec influxdb influx query --org swee.net "<flux>"'` (no sudo —
  user is in the docker group), or
- the HTTP API `POST http://192.168.1.200:8086/api/v2/query?org=swee.net` with
  `Authorization: Token <influx-token>` + `Content-Type: application/vnd.flux`.
Climate measurement is `device_environment` (see §3 for fields/devices). A handy dev bearer token
for `*.swee.net` comes from the `swee:sweenet-auth` Claude skill (device-code flow).

**Mirror references (read these when extending):**
- `../countinghouse` — the closest template; greenhouse mirrors it minus energy. Its
  `internal/{config,influx,httpapi,testutil}` and `internal/energy/{window,interval,series,series_rows}.go`
  are what was copied/adapted into `internal/{config,influx,httpapi,testutil,climate}` here.
  Its `demo/` is the template for greenhouse demos. Its CI/deploy mirror ours.
- `../statehouse` — the canonical "how we build Go services here" + the WRITER of all the telemetry
  (`internal/influx/writer.go` shows the `device_environment` schema).
- `github.com/sweeney/identity/common` **v0.3.0** — `auth.JWKSVerifier` (inbound),
  `auth.TokenSource` (outbound, shared since v0.3.0), `spec.Converter`.

**Key correctness lesson already applied:** Influx `aggregateWindow` stamps the right edge by
default, shifting series one bucket late. Builders set `timeSrc:"_start"`; there's a regression
test. Do NOT remove it. (This bit countinghouse — see its history.)
