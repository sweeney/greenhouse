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
  max / last** — this is the defining difference from countinghouse's additive kWh. `group_by=location`
  means the **mean** across a room's sensors, not a total.
- **Bucket alignment.** Influx `aggregateWindow` stamps the right edge by default, which shifts every
  value one bucket late. Builders MUST set `timeSrc: "_start"` (left-edge), matching the Go-owned
  canonical bucket axis. This bit it in countinghouse — don't reintroduce it. See PLAN §3.
- **No energy concepts.** No cost, tariff, bill, counter/integral, or on/off events. Those belong to
  countinghouse. Climate fields are plain gauge readings.
- **Device selection is a class allowlist**, in ONE place: `climateClasses` +
  `DeviceConfig.ReportsEnvironment()` in `internal/config/device.go`. Currently `environmental_sensor`
  and `fire_alarm` (the alarms report `temperature_c`, and office/utility have no other sensor).
  Never re-introduce a per-package class const — that duplication is what this replaced. Known
  limitation, documented at the map: class asserts "every device of this class reports environment
  telemetry", so a future non-reporting model would yield an empty series until someone edits and
  deploys. The alternative (select on `environment_fields`) is a one-predicate change.
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
  (`/etc/greenhouse/config.yaml`). Greenhouse reads **`statehouse_devices` only** — it does NOT use
  `energy_tariffs`. Fetches are fail-open (log + keep last-known) with SIGHUP reload.
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
