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
  **mean / min / max / last** — never summed. `group_by=room` is the **mean**
  across a room's sensors, not a total.
- **Left-edge buckets.** Flux `aggregateWindow` is called with
  `timeSrc: "_start"` so values align to the Go-owned canonical bucket axis;
  empty buckets are `null` (no reading), not zero.
- **Grid-aligned bucket axis.** The canonical axis is snapped onto the interval
  grid anchored at **local midnight** — the same grid Influx's location-aware
  `aggregateWindow` uses. So a `6h` axis is 00/06/12/18 local in both GMT and
  BST, and every Influx bucket stamp exact-matches a Go bucket.

## HTTP API

Listen port **:8082** (statehouse :8080, countinghouse :8081). All times
Europe/London. `/healthz` and `/openapi.json` are public; everything else
requires a Bearer JWT (user **or** service token).

| Route | Returns |
|---|---|
| `GET /healthz` | status, version, uptime, influx_reachable, remote_config status |
| `GET /openapi.json` | the OpenAPI spec as JSON |
| `GET /devices` | climate device catalog: id, display_name, room, floor, class, and an `environment_fields` hint |
| `GET /devices/{id}/series` | single-device, single-field time-series |
| `GET /series` | multi-series; `group_by` device (default) or room (mean per room) |
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
- **Bucket labels are grid-aligned, so the first bucket may start before `from`.**
  A window whose start is off the interval grid (every `<N>h`, and a `custom`
  `from` that is not on the grid) has its first bucket widened back to the grid
  boundary containing `from`. That leading slice carries no in-window data — the
  query range still begins at `from` — and it is what keeps every bucket's value
  describing the span its label claims. `today`/`week`/`month`/`<N>d` start at
  local midnight and are already on every sub-day grid, so they are unchanged.
  Known caveat: a sub-day interval over a window that *crosses* a DST transition
  steps by fixed duration and drifts an hour off Flux's stretched local grid
  after the changeover.
- `interval` — `5m,15m,30m,1h,6h,1d`; smart default per window, ~2500-bucket cap
  (admits 5-minute resolution over a 7-day window, ~2016 buckets).
- `field` — one of `temperature_c, humidity_pct, pressure_hpa, wind_speed_ms,
  wind_dir_deg, rainfall_mm, illuminance_lux, uv_index`. Default `temperature_c`.
  Unknown field → 400.
- `fn` — `mean` (default) \| `min` \| `max` \| `last`. `sum` is deliberately not
  offered (non-additive). Bad fn → 400. `wind_dir_deg` is **circular** (a 0–360°
  bearing): arithmetic mean/min/max are wrong on an angular axis, so it accepts
  only `last` (and defaults to it); `mean`/`min`/`max` for it → 400.
- `group_by` — `device` (default) \| `room`. Bad value → 400.
- `devices` — (`/series` only) CSV of device ids to chart, e.g.
  `devices=sensor_b,sensor_c`. Restricts the series to those
  sensors (omit for all climate devices). An unknown or non-climate id → 400.
  Composes with `rooms` and `floors` as AND.
- `rooms` — (`/series` only) CSV of floorplan room ids to chart, e.g.
  `rooms=floor2.room-a,floor3.room-a`. The candidate set is always
  climate sensors only, so a non-climate device sharing a room is never included,
  and a room with no climate sensor → 400. Composes with `devices` and `floors` as AND.
- `floors` — (`/series` only) CSV of floors to chart, e.g.
  `floors=floor1,floor2`. The coarse sibling of `rooms`: it selects every
  climate sensor whose declared floor matches, so a caller does not have to enumerate
  the floorplan. A floor with no climate sensor → 400, and a device whose entry
  declares no floor is never selected. Composes with `devices` and `rooms` as AND.
- `shape` — `columns` (default, shared buckets axis + per-series arrays) \|
  `rows` (flat one-row-per-(series,bucket)). Both carry `field`/`unit`/`fn`.

### Which devices are charted

A device from `statehouse_devices` is charted when its **class** reports
environmental telemetry:

- `environmental_sensor` — the purpose-built climate sensors and the weather
  station.
- `fire_alarm` — the installed alarms write `temperature_c` alongside their
  smoke state. They are included because some rooms hold **no
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
  `devices=`/`rooms=` intersection that matches nothing.

A device declaring no `environment_fields` is treated as **unknown coverage**
and is never rejected or omitted. `environment_fields` is config, and config can
be stale — if a sensor starts reporting a new field before the namespace catches
up, greenhouse must not turn that oversight into a data outage. It only ever
narrows on a positive declaration.

## Rooms

`location` used to mean two different things across these services — a geographic site
and a room — so rooms are now `room`, sites are `site`, and floors are `floor`. Room ids
are `<floor>.<slug>`: `floor2.room-a`, `floor3.room-a`.

The deprecated `location` spelling has been removed: `group_by=location`, `locations=`
and the `location` response field are all gone. Use `group_by=room`, `rooms=` and `room`.

Greenhouse reads whichever the devices namespace carries. A namespace still declaring
`location` keeps working untouched, which is what lets the namespace and its consumers
migrate independently.

`/devices` also reports a `floor` per device. The devices namespace declares `floor`
as a **first-class property** alongside `room`, and greenhouse passes it through
unchanged. It does **not** derive the floor from the room id: the floorplan owns that
fact, and re-deriving it here would be a second implementation of someone else's
taxonomy that disagrees the moment a room id is spelled unexpectedly. A device whose
entry declares no floor is UNKNOWN — reported as `""`, and never matched by `floors=`
— rather than guessed at.

`/series` accepts `floors=` for the same vocabulary, so a floor read off the catalog
can be handed straight back as a filter. Grouping is still `device` or `room` — there is
no `group_by=floor` **yet**. Chart the rooms on a floor with `floors=…&group_by=room`,
noting that `group_by=room` keys on rooms, so a device with a declared floor but no room
id is absent from that view; `group_by=device` charts it.

Floor grouping is deferred, not rejected. The objection to it — that a floor-wide mean
averages rooms of wildly different character into a number describing nowhere — argues
against a *hardcoded* floor mean, and `group_by=room` already applies exactly that
hardcoded cross-member mean with no way for a caller to ask for anything else. The fix
is one the room case needs too: a `group_fn` (`mean`/`min`/`max`, applied after `fn`)
that lets a caller say which question they are asking, so a floor can render as a
min–max band with the mean through it and heterogeneity shows up as the band's width.
That is a larger change than a new filter, so it is tracked separately.

## Config

The devices namespace is named by config, so a site reads its own:

```yaml
site:
  id: home
  devices_namespace: devices_home
```

`devices_namespace` is **required — there is no default.** It once fell back to the
shared `statehouse_devices` document, but that was deleted from the config service on
2026-08-12, so a fallback would name a 404: fetched fail-open into an empty snapshot,
with `/healthz` still reporting `ok` and every endpoint honestly serving zero devices.
Greenhouse refuses to start when it is unset instead, because boot is the only place
that can still tell "unnamed" apart from "named and empty".


Local YAML (`/etc/greenhouse/config.yaml`, see `config/config.example.yaml`):
`http.listen`, `influx{url,org,bucket,token_file}`,
`identity{base_url,client_id,client_secret}`, `remote_config.base_url`,
`site{id,devices_namespace}`, `house.timezone`, `auth.allow_insecure`. Device inventory
is fetched from the remote config service (one devices namespace only — **no** tariffs,
and `site.devices_namespace` must name it). Fetches are fail-open
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
