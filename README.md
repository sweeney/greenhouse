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
| `GET /healthz` | status, version, uptime, influx_reachable, resolved site (incl. namespaces), remote_config status |
| `GET /openapi.json` | the OpenAPI spec as JSON |
| `GET /devices` | climate device catalog: id, display_name, room, floor, class, and an `environment_fields` hint |
| `GET /floors` | floor catalog: id, name, order, elevation, device_count — the vocabulary `floors=` accepts |
| `GET /rooms` | room catalog: id, name, floor, category, area, device_count — the vocabulary `rooms=` accepts |
| `GET /devices/{id}/series` | single-device, single-field time-series |
| `GET /series` | multi-series; `group_by` device (default), room or floor, combined per `group_fn` |
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
- `group_by` — `device` (default) \| `room` \| `floor`. A grouped series is
  **labelled** with the floorplan's name for that room or floor, falling back to the
  id when it declares none (or no floorplan namespace is configured). `key` is always
  the id — only `label` varies — so identity matching is unaffected. Bad value → 400. A device
  declaring no room (for `room`) or no floor (for `floor`) has UNKNOWN membership
  and is **omitted** rather than keyed on `""`; `group_by=device` charts it. A
  **circular** field cannot be combined across a group's members at all, so
  grouping one by a key whose group holds two or more climate sensors → 400.
- `group_fn` — how a group's **members** are combined: `mean` (default) \| `min`
  \| `max`. Applied *after* `fn` (see below). `last` → 400 (not a spatial
  statistic); `sum` → 400 (non-additive); supplying it with `group_by=device` →
  400, because that grouping combines nothing. `/devices/{id}/series` rejects it
  for the same reason — it always groups by device. (`group_by` itself and the
  `devices`/`rooms`/`floors` selectors *are* ignored there: the path segment has
  already selected, so those are redundant rather than impossible.)
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

## Aggregation

### Two aggregation axes: `fn` then `group_fn`

Greenhouse aggregates twice, and the steps are independent:

```
per device:  fn=        collapses a device's samples within a bucket  (Influx aggregateWindow)
per group:   group_fn=  combines the group's devices into one series  (Go, after the query)
```

`fn` runs **first**, inside Influx. `group_fn` runs **second**, here. They **do
not commute**:

- `fn=mean&group_fn=max` — "the warmest member's bucket average"
- `fn=max&group_fn=mean` — "the mean of each member's peak"

Both are legitimate questions with different answers, so the order is part of the
contract rather than something to infer.

`group_fn` defaults to `mean`, which is what greenhouse always did — every
request written before the parameter existed is unchanged. Neither axis offers
`sum`: climate is non-additive however it is sliced.

**Gaps.** Every combine skips a member that did not report, exactly as the mean
always did — a `min` that counted an absent sensor as `0` would report a freezing
room. A bucket nobody reported stays `null`, never a zero.

### Charting a floor

`group_by=floor` combines every sensor **declaring** that floor, across its rooms
(never a floor derived from the room id — the floorplan owns that fact). `GET /floors`
lists the floors available to chart, with their names and storey order.

The objection this used to be withheld for — that a floor-wide mean averages
rooms of wildly different character into a number describing nowhere — was an
argument against a *hardcoded* floor mean, and `group_by=room` applied exactly
that hardcoded mean itself. `group_fn` answers it properly: chart a floor three
times as `min`, `mean` and `max` and render it as a **band with the mean through
it**. A sun-facing room and a cold stairwell then show up as the band's *width*,
which is precisely the information a single mean loses.

A device declaring no floor is UNKNOWN and is **omitted** from a floor grouping,
exactly as a room-less device is omitted from `group_by=room`. Greenhouse neither
keys a series on `""` nor invents an "unknown" bucket: neither is a valid floor
id, and an invented key would be a value `floors=` rejects — `/series` would
advertise a vocabulary `/series` itself refuses. Chart such a device with
`group_by=device`.

### Circular fields are never combined

`wind_dir_deg` is a 0–360° bearing, and the arithmetic that is merely
*non-additive* for a temperature is outright **invalid** for an angle:
`mean(350°, 10°)` is `180°` — due South — though both readings say North.

`fn=` has always refused `mean`/`min`/`max` for such a field. The **cross-member
combine** applies exactly that arithmetic when a group holds more than one
sensor, so it is refused on the same grounds:

- Grouping a circular field where any group holds 2+ climate sensors → **400**,
  naming the field, the group and the way out. Said up front rather than served
  as gaps: `null` means "no reading", so a silently-gapped series would be
  indistinguishable from a sensor outage.
- A group with exactly **one** reporting member passes that bearing through
  unchanged — a single instantaneous bearing is always valid. This is why
  `group_by=device` is always answerable.
- `min`/`max`/`mean` are **null** for a circular series. They are linear
  statistics: a legend reading "min 10°, max 350°" describes a 20° spread as
  though it were 340°. The `values` themselves are still served — refusing to
  summarise is not refusing to chart.

Proper vector averaging (the mean of unit vectors) would let both the
multi-member case and the summary be answered honestly. Until it lands,
greenhouse refuses rather than emitting a confident-but-wrong bearing.

## Rooms and floors

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

`GET /floors` publishes the floor records themselves — `id`, `name`, `order`,
`elevation` and a `device_count` — so a client stops re-deriving two things it cannot
know: **storey order** (floor ids do not sort into building order, so every consumer
that guesses is wrong the moment it meets a different building) and the **display
name** (which consumers otherwise title-case from the id and hope). It is the same
re-derivation `floor` itself removed, one level up: a second implementation of the
floorplan's taxonomy living in every client instead of in one service.

Which floors are listed is the endpoint's real contract: **exactly the floors a climate
device declares**, which is exactly the set `floors=` accepts. A picker filled from
`/floors` therefore cannot produce a 400. A floorplan record for a floor with no climate
sensor is *not* listed — it exists in the building, but not as far as the climate API is
concerned — and a floor devices declare but the floorplan has no record for *is* listed,
with `name` empty and `order` null. Those are nullable rather than defaulted because
greenhouse reports UNKNOWN where it has no answer; it never invents a label or a
position, for the same reason it never derives a device's floor from its room id.

Rows come back in declared `order` ascending, then by id, so undeclared ones sort last
and the list renders top to bottom without a client re-sorting it.

`GET /rooms` is the room-shaped sibling, and the same argument one level down.
`group_by=room` used to label every series with the bare room id, so every client wrote
the same function — split on the dot, replace hyphens, title-case, hope — which is wrong
for any room whose display name is not a mechanical transform of its slug and silently
stops matching the floorplan the moment a room is renamed. It also publishes each room's
**`category`** (`kitchen`, `circulation`, `plant`, …), which is how a client tells a
plant room from a living space without matching substrings against the id.

`category` is relayed **raw** and deliberately not reduced to a computed flag like
`is_living_space`. Whether a plant room "counts" is a per-client policy question, not a
fact about the room: a floor-mean view excludes it, a "where is the heat going?" view
wants it, and an equipment view wants only it. A boolean would bake the first caller's
answer into the API and leave the other two working around it. The floorplan owns the
taxonomy, greenhouse relays it, clients interpret it.

Which rooms are listed follows `/floors` exactly: the rooms at least one climate device
sits in, which is the set `rooms=` accepts, so a picker built from it cannot 400. Room
`name`s are **not unique** — two rooms on different floors may share one — so `id` is the
key, and a client wanting an unambiguous label joins `/rooms` to `/floors` on the room's
`floor`.

That join **can miss**, and a client must handle it. `/floors` lists the floors a climate
*device* declares; a room's `floor` is what the *room record* declares. Greenhouse does not
arbitrate between two upstream declarations, so the two diverge whenever a device has a
room but no declared floor — `/rooms` will name a floor that `/floors` omits and `floors=`
rejects with a 400. Fall back to the room name alone. For the same reason, a floor's rooms'
`device_count`s need not sum to that floor's `device_count`.

`/series` accepts `floors=` for the same vocabulary, so a floor read off the catalog
can be handed straight back as a filter, and `group_by=floor` charts it as one line
(see **Charting a floor**). A device with a declared floor but no room id is absent
from `group_by=room` — it has no room to be keyed on — and `group_by=floor` or
`group_by=device` charts it.

## Config

The devices namespace is named by config, so a site reads its own:

```yaml
site:
  id: home
  devices_namespace: devices_home
  floorplan_namespace: floorplan_home   # optional
```

`devices_namespace` is **required — there is no default.** It once fell back to the
shared `statehouse_devices` document, but that was deleted from the config service on
2026-08-12, so a fallback would name a 404: fetched fail-open into an empty snapshot,
with `/healthz` still reporting `ok` and every endpoint honestly serving zero devices.
Greenhouse refuses to start when it is unset instead, because boot is the only place
that can still tell "unnamed" apart from "named and empty".

`floorplan_namespace` is **optional**, and deliberately so. Greenhouse charts devices; a
room or floor's label, storey order and category are presentation detail. Unset, `/floors`
and `/rooms` still list everything that holds a climate sensor — with `name`, `order` and
`category` reported as unknown, and grouped series still labelled by id — and a fetch
failure is fail-open and never touches the devices snapshot. A missing floorplan can
degrade the labels; it can never stop a climate service serving climate.

The floorplan document is an **array**, unlike the devices namespace's map keyed by id:

```json
{
  "floors": [ { "id": "floor1", "name": "Lower Floor", "order": 1, "elevation": 0.0 } ],
  "rooms":  [ { "id": "floor1.room-a", "name": "Room A", "floor": "floor1",
                "category": "utility", "area": 12.4 } ]
}
```

It carries the building's **rooms** in the same shape, under a `rooms` key. Greenhouse
also accepts the devices-style `{"<id>": {…}}` form — which carries floors only — because
the namespace belongs to another service and greenhouse cannot deploy in lockstep with it. Unmodelled
keys are ignored. Assuming it matched the devices shape is what silently broke `/floors`
in prod (#28) — the decode failed, fail-open kept an empty snapshot, and the response was
byte-identical to a namespace nobody had configured.

`/healthz`'s `site` block reports it alongside `devices_namespace` (omitted when unset),
so an operator seeing blank floor names can tell "no floorplan namespace configured" from
"configured, first fetch hasn't landed" — a distinction `remote_config` only makes after a
fetch has been attempted.


Local YAML (`/etc/greenhouse/config.yaml`, see `config/config.example.yaml`):
`http.listen`, `influx{url,org,bucket,token_file}`,
`identity{base_url,client_id,client_secret}`, `remote_config.base_url`,
`site{id,devices_namespace,floorplan_namespace}`, `house.timezone`, `auth.allow_insecure`. Device inventory
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
