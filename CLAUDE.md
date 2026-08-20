# Greenhouse — Claude guidance

Greenhouse is a read-side **climate / environment reporting** service for the swee.net home.
It turns the per-sensor environmental telemetry statehouse writes to InfluxDB (`device_environment`:
temperature, humidity, pressure, wind, rainfall, light, UV) into **time-series charts** over
arbitrary windows, for indoor sensors and the outdoor weather station. **Full design: `PLAN.md`.**

It is the **climate sibling** of `countinghouse` (energy cost) and `statehouse` (real-time state).
Mirror their conventions: `../countinghouse` is the closest template (greenhouse mirrors it minus
the energy bits) and `../statehouse` is the canonical "how we build Go services here". Reuse
countinghouse's domain-agnostic scaffolding (windowing, interval/bucket axis, columnar/rows series
shapes, the Influx Querier + fake, the Server/auth/CORS/spec skeleton, the config Fetcher).

## Core invariants (don't violate without discussion)

- **Read-side only.** No MQTT, no device ingest, no real-time state. Query Influx + shape for charts.
- **Stateless w.r.t. accumulation.** Derive answers on query; survive restart with zero data loss.
  The durable truth is statehouse's writes to Influx. Any cache must be rebuildable from Influx.
- **Climate is NON-additive.** You never *sum* temperatures. Bucket and group with **mean / min /
  max / last** — this is the defining difference from countinghouse's additive kWh. `group_by=room`
  means the **mean** across a room's sensors, not a total.
- **Bucket alignment.** Influx `aggregateWindow` stamps the right edge by default, which shifts every
  value one bucket late. Builders MUST set `timeSrc: "_start"` (left-edge), matching the Go-owned
  canonical bucket axis. This bit it in countinghouse — don't reintroduce it. See PLAN §3.
- **No energy concepts.** No cost, tariff, bill, counter/integral, or on/off events. Those belong to
  countinghouse. Climate fields are plain gauge readings.
- **Device selection is a class allowlist**, in ONE place: `climateClasses` +
  `DeviceConfig.ReportsEnvironment()` in `internal/config/device.go`. Currently `environmental_sensor`
  and `fire_alarm` (the alarms report `temperature_c`, and some rooms have no other sensor).
  Never re-introduce a per-package class const — that duplication is what this replaced. Known
  limitation, documented at the map: class asserts "every device of this class reports environment
  telemetry", so a future non-reporting model would yield an empty series until someone edits and
  deploys. The alternative (select on `environment_fields`) is a one-predicate change.
- **Floor is config, not derivation.** The devices namespace declares `floor` as a
  first-class property alongside `room`. `DeviceConfig.Floor` mirrors it and greenhouse
  passes it through; never re-derive a floor from the room id's `<floor>.<slug>` shape.
  The floorplan owns that fact, and a second implementation of it here would disagree
  the moment a room id is spelled unexpectedly. An undeclared floor is UNKNOWN: the
  catalog reports `""` and `floors=` never matches it.
- **Two aggregation axes, applied in order and NOT commutative.** `fn=` collapses one
  device's samples within a bucket (Influx `aggregateWindow`); `group_fn=` combines a
  group's devices (Go, in `buildSeries`). `fn=mean&group_fn=max` and `fn=max&group_fn=mean`
  are different legitimate questions — document the order, never leave it to be inferred.
  `group_fn` defaults to `mean` (what greenhouse always did), rejects `last` (across
  members that is "whichever sensor reported most recently", not a spatial statistic),
  and rejects being supplied with `group_by=device` (which combines nothing). No `sum`
  on either axis. Never hardcode a cross-member combine again — that silent mean is what
  made the `group_by=floor` omission indefensible (#23).
- **UNKNOWN group membership is OMITTED, never keyed on `""` and never bucketed as
  "unknown".** A device declaring no room (`group_by=room`) or no floor (`group_by=floor`)
  is absent from that view and charted by `group_by=device`. `""` is not a valid id, and
  an invented key would be a value `rooms=`/`floors=` reject — `/series` would advertise a
  vocabulary `/series` itself refuses. Settled identically on both axes in
  `climate.assembleByGroup`; do not answer it differently for a future third axis.
- **Circular fields are never combined, on EITHER axis.** `wind_dir_deg` is a 0–360°
  bearing: `mean(350°, 10°)` is `180°` (South) when both readings say North. `fn=`
  refuses the linear aggregations for it, and so must the **cross-member combine** —
  `AssembleSeries` therefore takes `field`. A group with 2+ climate sensors 400s up
  front (`climate.CircularGroupConflict`), a single-member bucket passes its bearing
  through, and `min`/`max`/`mean` are null because linear summaries are undefined on
  an angular axis. Never re-introduce a combine that cannot see the field — that
  blindness is the bug this fixed (#24). Grouping is defined once, in
  `climate.GroupKeyFor`: two implementations of "which devices share a series" would
  drift, and what drifts is which readings get averaged together.
- **`/floors` lists the FILTER vocabulary, not the floorplan.** It serves exactly the
  floors at least one climate device declares — the same set `floors=` accepts — so a
  picker filled from it can never produce a 400. A floorplan record for a floor with no
  climate sensor is omitted; a declared floor with no record is listed with `name` empty
  and `order` null. Never build this listing from the floorplan namespace instead: the two
  endpoints would then disagree, which is the failure `/devices`↔`/series` is already
  tested against. `name`/`order`/`elevation` are passed through and nullable — 0 is a real
  order (a basement) and 0.0 a real elevation, which is why `Order`/`Elevation` are
  pointers.
- **The floorplan namespace is an ARRAY, not a keyed map** — unlike the devices
  namespace, and this is the one fact about it that is easy to get wrong. It publishes
  `{"floors": [{"id": …, "name": …, "order": …}, …]}`, each record carrying its own id.
  `config.floorplanDocument` also accepts the devices-style `{"<id>": {…}}` because the
  namespace belongs to another service greenhouse cannot deploy in lockstep with, and it
  discriminates on JSON *type* rather than key name (a `floors` key holding an OBJECT is a
  floor whose id is `floors`). Assuming it matched the devices shape silently broke
  `/floors` in prod (#28): the decode failed, fail-open kept an empty snapshot, and the
  response was byte-identical to an unconfigured namespace. Never re-derive the shape from
  the devices namespace — that guess is what this documents against.
- **`site.floorplan_namespace` is OPTIONAL** (unlike `devices_namespace`, which is required
  with no default). Greenhouse charts devices; floor labels are presentation detail. Unset
  is silent — no request, no `/healthz` status — and a configured-but-failing floorplan is
  fail-open and must never touch the devices snapshot. This is the second namespace
  greenhouse reads; keep it strictly optional so the "one required namespace" boot
  guarantee is unchanged.
- **`environment_fields` is a hint, and staleness must not lose data.** It declares what a device
  writes to `device_environment`. Where declared, `/devices/{id}/series` 400s on a field the device
  does not report and `/series` omits such devices; where **undeclared**, coverage is UNKNOWN and
  greenhouse never rejects or omits (`DeviceConfig.MayReportField`). Only ever narrow on a positive
  declaration — config lags reality, and a stale namespace must not hide real readings.

## House rules

- **gofmt:** run `gofmt -w` on changed Go files before committing. CI enforces it.
- **OpenAPI:** when adding/removing an HTTP endpoint, update `internal/httpapi/openapi.yaml`.
  A path-coverage test (`internal/httpapi/spec_test.go`) fails CI if routes and spec drift.
- **Docs stay in sync:** any change to endpoints, request/response shapes, config, or behaviour
  updates BOTH `internal/httpapi/openapi.yaml` AND `README.md` in the same change. Stale docs = bug.
- **TDD for bug fixes:** write a failing test reproducing the bug, confirm red, then fix to green.
- **Tests matter:** match countinghouse/statehouse density. Use fake doubles (fake Influx query
  client) and an injected clock — never call `time.Now()` in logic. `make test` = `go test -race -count=1 ./...`.
- **Issues:** close via `Closes #N` in the commit message, not `gh issue close`.
- **Deploy:** only when the user asks. First-time host setup is the self-contained
  `deploy/bootstrap-greenhouse.sh` (`sudo bash` on garibaldi); then `make deploy`
  (= `./deploy/deploy.sh sweeney@garibaldi`, SSH+systemctl). Listen port **:8686**
  (statehouse :8080, countinghouse :8585), public at `https://greenhouse.swee.net`.
  Locally: build the binary; no tmux/systemctl.

## Config & auth (see PLAN §7)

- Config is remote at `config.swee.net` (`GET /api/v1/config/{namespace}`), overlaid on local YAML
  (`/etc/greenhouse/config.yaml`). Greenhouse reads **one REQUIRED devices namespace** — named by
  `site.devices_namespace`, which is **required with no default** (the shared `statehouse_devices`
  document was deleted, so an unset namespace refuses to boot rather than serving zero devices
  while looking healthy) — plus the OPTIONAL `site.floorplan_namespace` (floor names and storey
  order for `/floors`; unset is silent and never affects the devices snapshot). It does NOT use
  `energy_tariffs`. `/healthz`'s `remote_config` block is keyed by whichever namespaces are
  actually read. Fetches are fail-open (log + keep last-known) with SIGHUP reload.
- Auth via `github.com/sweeney/identity/common` **v0.3.0**: `auth.JWKSVerifier` verifies inbound
  tokens (JWKS); `auth.TokenSource` is the shared outbound `client_credentials` source (no local
  copy — it was extracted to common in v0.3.0). **Accept service tokens** (`ParseServiceToken`) as
  well as user tokens in the inbound middleware — greenhouse is likely called by other services.
- Greenhouse needs its own Influx **read token** (scoped to the `statehouse` bucket) and its own
  `client_id`/`client_secret` registered in id.swee.net.

## Shared-library follow-up

Greenhouse intentionally **copies** countinghouse's generic time-series scaffolding for now (the
established mirror pattern). Once greenhouse is real, the plan is to extract that common core into a
shared `github.com/sweeney/timeseries` module consumed by both — factored from two implementations,
not one. Prioritise de-duplicating the subtle, correctness-critical bits (window/DST, bucket
alignment) where copy-drift is dangerous. See `PLAN.md` §10.
