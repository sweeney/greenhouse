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
| `GET /devices` | climate device catalog: id, display_name, location, class, and an `environment_fields` hint |
| `GET /devices/{id}/series` | single-device, single-field time-series |
| `GET /series` | multi-series; `group_by` device (default) or location (mean per room) |
| `GET /devices/{id}/latest` | the device's most recent reading across its fields (within the last 7 days) |
| `GET /fields` | the field registry (name, unit, default fn) |

### Series parameters

- `window` — `today` (default) \| `week` \| `month` \| `custom`, or a **rolling**
  spec `<N>d` / `<N>h` (e.g. `7d`, `30d`, `24h`). `from`/`to` (RFC3339) are valid
  **only** with `custom` — required there, and a 400 for any other window
  (period-to-date or rolling, which derive their own range). A span over ~2 years
  (Influx retention) → 400.
- **Rolling windows:** `<N>d` is a trailing N calendar days ending now,
  **day-aligned to local midnight** — `7d` = today + the previous 6 days, `1d` ≡ `today`;
  `<N>h` is an **exact** trailing N hours (e.g. `24h`), not midnight-aligned. Use these for
  "last 7 days" / "last 30 days" (`7d`/`30d`), as distinct from `week`/`month`, which reset
  on Monday / the 1st.
- `interval` — `5m,15m,30m,1h,6h,1d`; smart default per window, ~1000-bucket cap.
- `field` — one of `temperature_c, humidity_pct, pressure_hpa, wind_speed_ms,
  wind_dir_deg, rainfall_mm, illuminance_lux, uv_index`. Default `temperature_c`.
  Unknown field → 400.
- `fn` — `mean` (default) \| `min` \| `max` \| `last`. `sum` is deliberately not
  offered (non-additive). Bad fn → 400. `wind_dir_deg` is **circular** (a 0–360°
  bearing): arithmetic mean/min/max are wrong on an angular axis, so it accepts
  only `last` (and defaults to it); `mean`/`min`/`max` for it → 400.
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

### Which devices are charted

A device from `statehouse_devices` is charted when its **class** reports
environmental telemetry:

- `environmental_sensor` — the purpose-built climate sensors and the weather
  station.
- `fire_alarm` — the installed alarms write `temperature_c` alongside their
  smoke state. They are included because some rooms (office, utility) hold **no
  `environmental_sensor` at all**, so without them those rooms have no climate
  coverage despite live data in Influx.

`class` is reported as-is on `/devices`, so a consumer can tell a purpose-built
sensor from an alarm and weight them differently if it wants to.

This is a **class allowlist**, which asserts that every device of these classes
reports environment telemetry. That holds for the current fleet, but a future
fire alarm model that does not report temperature would still be listed and
would return a well-formed, permanently empty series; correcting that means
editing `climateClasses` in `internal/config/device.go` and redeploying. The
alternative — selecting on a non-empty `environment_fields` — would push the
decision entirely into config; see that file's comment for the trade-off.

### The `environment_fields` hint

`environment_fields` on `/devices` comes from the device config key of the same
name in `statehouse_devices`, otherwise it falls back to the full field
registry. **The fallback over-advertises:** greenhouse can't know per-device
coverage without querying Influx, so a device whose config omits the key appears
to offer every field. Populating `environment_fields` in the namespace is what
makes the catalog honest.

Where config declares it, coverage is also **enforced on the series endpoints**,
with two deliberately different answers:

- `GET /devices/{id}/series` — a field the named device does not report is a
  **400**. The endpoint promises exactly one series, so an impossible request
  is an error; answering 200 with all-null buckets would be indistinguishable
  from a sensor outage (null means "no reading").
- `GET /series` — devices that cannot report the field are **omitted** rather
  than padding the response with all-null lines. `field=pressure_hpa` returns
  the one series that has data, not one real line and nine empty ones. If the
  filter leaves nothing, that's a valid empty `200`, consistent with a
  `devices=`/`locations=` intersection that matches nothing.

A device declaring no `environment_fields` is treated as **unknown coverage**
and is never rejected or omitted. `environment_fields` is config, and config can
be stale — if a sensor starts reporting a new field before the namespace catches
up, greenhouse must not turn that oversight into a data outage. It only ever
narrows on a positive declaration.

## Config

Local YAML (`/etc/greenhouse/config.yaml`, see `config/config.example.yaml`):
`http.listen`, `influx{url,org,bucket,token_file}`,
`identity{base_url,client_id,client_secret}`, `remote_config.base_url`,
`house.timezone`, `auth.allow_insecure`. Device inventory is fetched from the remote config service
(`statehouse_devices` namespace only — **no** tariffs). Fetches are fail-open
(log + keep last-known) with SIGHUP reload. Greenhouse needs its own Influx
**read token** (statehouse bucket) and `client_id`/`secret` in id.swee.net.

**Secure by default:** inbound auth is disabled only when `identity.base_url` is
empty (local dev/tests), and that path is loud — a startup warning is logged and
`/healthz` reports `"auth":"disabled"`. To actually boot unauthenticated you must
set `auth.allow_insecure: true`; otherwise greenhouse refuses to start, so a
missing/typo'd `identity.base_url` can't silently expose the data API.

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

Runs on garibaldi as a hardened systemd unit, listening on **:8686**
(statehouse :8080, countinghouse :8585), behind `https://greenhouse.swee.net`.

First-time host setup is one self-contained script — copy nothing else, it embeds
the config, unit and sudoers rule and mints the read-only Influx token itself:

```
scp deploy/bootstrap-greenhouse.sh sweeney@garibaldi:/tmp/
ssh -t sweeney@garibaldi 'sudo bash /tmp/bootstrap-greenhouse.sh'   # prompts for the client_secret
```

It enables but does not start the service (no binary yet). Then deploy from the
dev machine:

```
make deploy   # = ./deploy/deploy.sh sweeney@garibaldi
```

`deploy.sh` builds linux/amd64, uploads a timestamped binary, symlinks it active
(keeping the last 3 for rollback), restarts the unit, and verifies both the
on-host health and that `https://greenhouse.swee.net/healthz` serves the deployed
commit.

## Follow-up

The generic time-series scaffolding (windowing, interval/bucket axis,
columnar/rows shapes, Influx Querier+fake, Server/auth/CORS/spec skeleton,
config Fetcher) is **copied** from countinghouse. The plan is to extract the
common core into a shared `github.com/sweeney/timeseries` module once greenhouse
is real (PLAN.md §10).
