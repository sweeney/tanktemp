# tanktemp: plan

Status: **first version built overnight on 2026-10-31, local only** (nothing
pushed). Phases 0–8 and 10 are done; phase 9 (deploy) and phase 11 (the rig)
are under way. See "Build status" at the end, and `GUESSES.md` for every
choice made without asking. The plan below is as agreed, with the decisions
taken during the build folded in.

## What it is

A daemon for a Raspberry Pi that reads DS18B20 1-Wire sensors fitted to one or
more unvented hot water cylinders. It publishes each sweep to MQTT and serves
`/healthz` and a status page. Any number of tanks, and any number of sensors
on each, is set in config.

## Decisions (2026-10-31, with the user)

1. **The sensor layout lives in a local YAML file on the Pi**, at
   `/etc/tanktemp/config.yaml`. That covers which ROM sits at which position,
   heights, and calibration. The layout is tied to the hardware, and must
   work at an offline boot. tanktemp never reads config.swee.net, so no OAuth
   client is needed on the Pi.
2. **What a tank *is* lives in `devices_<site>`** (decision 9). The local YAML
   only names the tank's device ID.
3. **Scope: the tanktemp service, plus a full written spec for statehouse**
   (`docs/STATEHOUSE.md`), so statehouse can build its own adapter. No
   statehouse or greenhouse code in this plan.
4. **Topics are under `energy/hot_water/`:** `energy/hot_water/{device_id}/state`.
5. **Raw readings and their quality only.** tanktemp publishes the calibrated
   °C, the raw °C, the error kind, attempts and bus for each sensor. Stored
   energy, "hot water available", stratification and neighbour-divergence
   detection are left to consumers. This is the "facts relayed raw" rule from
   greenhouse.
6. **tanktemp does not extend `common/iot`.** `iot` stays GPIO on/off only,
   as decided 2026-10-25. What the two do share is the runtime around the
   work, and that goes into its own module (decision 7), not into either
   service. Copying and pasting it is out.
7. **Three pieces go into `sweeney/common`** as part of this work:
   - `lifecycle`: signals, STARTUP, the ready heartbeat, scheduled
     heartbeats, SHUTDOWN with a reason, and cancellable work. It is pulled
     out of `iot.Run`, and `iot` is rebuilt on top of it. Consumers: `iot`
     (so boiler-sensor) and tanktemp.

   I build all three modules and tanktemp's use of them. **boiler-sensor and
   pi-price-screen migrate in their own sessions** (decided 2026-10-31),
   from sweeney/boiler-sensor#6 and sweeney/pi-price-screen#3. Both issues
   are blocked until the tags exist; I comment on them when each tag is
   pushed.
   - `health`: the always-200 `/healthz` (boiler-sensor, pi-price-screen),
     plus `health/sysinfo`, which tanktemp needs.
   - `mqttpub`: the resilient publisher and retry that ups-mqtt and netmon
     carry as copies (PLAN.md next candidate #2).
8. **The middle path for `iot`** (follow-up, after tanktemp ships):
   `iot/mqtt` moves onto `mqttpub`, and `iot`'s healthz and pi-helper code onto
   `health`, with boiler-sensor's goldens guarding the bytes. A numeric reader
   in `iot` is revisited only when a second numeric sensor exists.
9. **Tanks get a taxonomy in `common/devices`**, in a follow-up to PR #7 (not
   inside it):
   - **Generic asset fields on every device:** `manufacturer`, `model`,
     `installed`.
   - **A typed `cylinder` block** for the physical facts.
   - **statehouse reads and relays** them on its device API. That gives the
     fields a real reader, as the devices design requires.

   - **`telemetry: false`** on entries that send nothing themselves, so
     `Check` and consumers know the absence of an identity is deliberate.
   - **A generic `part_of` relation** (confirmed): each probe is its own
     device, class **`appliance_probe`**, with `part_of` pointing at the
     cylinder. The fridge and freezer probes can use the same model.
   - The class's docs are widened to cover cylinder probes:
     sweeney/statehouse#79 and sweeney/greenhouse#45. Neither needs a code
     change.
10. **Calibration is a single `offset_c` per sensor, 0 for now.** The 20/60 °C
    bath is optional and not on the path. Until offsets are measured, the docs
    and `/api/status` say plainly that readings are uncalibrated: ±0.5 °C
    absolute, with differences between probes good to the same ±0.5 °C.
11. **The prototype rig is the production Pi.** I may test on it, and may stop
    `templog` to do so. **Nothing I do may leave it unable to boot or
    unreachable.** That means read-only `--check` runs first, no edits to
    `/boot/firmware/config.txt` (the rig's overlays are already there, so
    `setup-pi.sh` should find nothing to change there), no reboots without
    asking, and everything I change on it is reversible.
12. **The deploy `lib.sh` is not centralised.** tanktemp copies
    boiler-sensor's, and the copy is recorded in common's PLAN.md.

## Design

### Go layout

```
cmd/tanktemp/            main.go: flags (-config, -version, -check-config), wiring, nothing else
cmd/w1capture/           read-only: snapshots /sys/bus/w1/devices into a fixture directory
internal/reading/        the domain boundary: Reading, Code (stable error codes), Topology,
                         and the Source interface. Fake + contract suite in reading/readingtest
internal/onewire/        the only code that knows about sysfs or DS18B20s; implements
                         reading.Source. From the prototype, plus context cancellation,
                         the 85 °C policy and the family seam
internal/config/         Load + Validate the YAML (takes bytes, not a path); Tank and Sensor
internal/sweep/          config + reading.Source + Clock → Sweep (per tank, ordered,
                         calibrated). Pure
internal/payload/        the wire contract: topics + JSON encoders. Golden tests live here
internal/startup/        startup report from reading.Source.Inspect + a SyncChecker
internal/status/         thread-safe snapshot (last sweep, startup report, MQTT state)
internal/web/            /, /api/status, /healthz (via common/health)
deploy/                  lib.sh (from boiler-sensor), tanktemp.service, config.example.yaml
deploy.sh  setup-pi.sh  deploy_test.sh
docs/ONEWIRE.md          moved from prototype/
docs/WIRE.md             the MQTT contract, normative
docs/STATEHOUSE.md       the adapter spec for statehouse
```

There's no `internal/daemon`. `main.go` hands `common/lifecycle` a work
function (one sweep), a ready check (first full sweep done) and an emitter
(format with `payload`, publish with `mqttpub`). Every piece of that is
tested on its own, and the run loop itself is tested once, in common.

**The boundaries, which are the point of the layout.** The core packages
(`config`, `sweep`, `payload`, `startup`, `status`) see the outside world only
through these interfaces:

| Boundary | Interface | Real | Fake |
|---|---|---|---|
| 1-Wire sensors | `reading.Source` | `onewire.Source` | `readingtest.Fake` |
| MQTT | `Publisher` (declared where it's used) | `mqttpub.MQTTPublisher` | `mqttpub.Fake` |
| Time | `Clock` (`Now`) | `time.Now` | fixed or stepped clock |
| Clock sync | `SyncChecker` | the `/run/systemd/timesync` flag | a bool |
| System info | `fs.FS` + `Statfs` | `/proc`, `/sys` | `fstest.MapFS` |

`reading.Source` is deliberately small, and says nothing about files, buses
as directories, or scratchpads:

```go
type Source interface {
	Discover(ctx context.Context) (Topology, error)                    // ROM → bus now; unexpected ROMs
	Read(ctx context.Context, t Topology, roms []string) map[string]Reading // retries and parallelism are the Source's job
	Inspect(ctx context.Context, rom string) (Info, error)            // ext_power, resolution: startup only
}

type Reading struct {
	ROM      string
	Bus      string // diagnostic only
	RawMilliC *int  // nil unless Code == OK
	Code     Code   // OK, NotPresent, BusLow, CRC, PowerOnReset, BadScratchpad, OutOfRange, Malformed, ReadError
	Suspect  string // "85c_unverified" or ""
	Attempts int
	Took     time.Duration
}
```

`onewire`'s typed errors become `Code`s in exactly one place, inside
`onewire`. Any error it doesn't recognise becomes `ReadError` rather than
gaining a branch of its own, so an unforeseen kernel error is reported, not
guessed at.

**Enforced, not just intended:** `boundaries_test.go` at the repo root runs
`go list -deps` and fails if any core package imports `os`, `io/fs`, `net`,
`syscall`, Paho, `common/mqttpub` or `internal/onewire`. Only `main.go`,
`onewire`, `web` and `cmd/w1capture` touch the outside world.

Dependencies: `gopkg.in/yaml.v3`, `common/lifecycle`, `common/mqttpub`
(Paho), `common/health`. Otherwise stdlib. `CGO_ENABLED=0`. Go 1.26, as
pi-price-screen.

### Config

```yaml
w1_root: /sys/bus/w1/devices        # default
sweep:
  interval: 30s
  attempts: 3
  parallel: true                    # one goroutine per bus
clock_sync_wait: 2m                 # then sweep anyway, flagged clock_synced:false
heartbeat: 15m
http:
  listen: ":80"
mqtt:
  broker: tcp://broker.lan:1883
  client_id: tanktemp-<host>
  topic_prefix: energy/hot_water    # default
  username: ""                      # optional; password_file if set
tanks:
  - device_id: hot-water-cylinder   # the devices_<site> key; also the topic segment
    sensors:
      - rom: 28-0000117287e6
        position: top               # [a-z0-9_]+, unique within the tank
        order: 1                    # explicit; 1 = top
        height_mm: 1500             # optional; mm, matching the devices cylinder block
        offset_c: 0                 # optional, default 0; added to the raw reading
```

`device_id` is the single name for the tank: it's the key in `devices_<site>`,
the topic segment, and the prefix of each probe's `primary`. There's no
separate local tank name that could drift from it. Display name, room and
dimensions are in `devices_<site>`, not here.

`Validate` refuses to start the service on any of these, and names the line
where it can:
- a ROM that isn't `28-` plus 12 hex digits;
- a ROM used twice anywhere;
- a `device_id` that isn't `[a-z0-9_-]+` (it must be safe as a topic
  segment);
- duplicate device IDs, or duplicate positions within a tank;
- `order` values in a tank that aren't unique and contiguous from 1;
- `order` and `height_mm` that disagree, when both are given: the cross-check
  is on two declared facts, and neither is derived from the other;
- unknown keys (`yaml` `KnownFields(true)`, since a typo in a local file
  should fail loudly);
- an interval shorter than the worst-case sweep (0.8 s × attempts × the most
  sensors on one bus).

`tanktemp -check-config` runs the same validation and exits. `setup-pi.sh`
and `deploy.sh --check` use it.

### Sweep and reading rules

These come straight from ONEWIRE.md:

- Sensors are identified by ROM. Bus names are diagnostic only.
- `w1_slave` with the scratchpad checks: bus-low, CRC, bad scratchpad,
  out of range. A power-on reset is raw 0x0550 **and** byte 6 = 0x0C. A real
  85 °C is accepted.
- **Interim 85 °C policy, until a power-on-reset capture exists:** a reading
  of exactly 85.000 °C that passes every check is retried once more. If it
  persists, it is published with `"suspect":"85c_unverified"` and not dropped.
  This flag is removed once phase 11 has captured a real power-on reset.
- Up to 3 attempts; `not_present` is not retried.
- Buses are read in parallel. Between reads, `ctx` is checked, so shutdown
  waits for at most one read per bus (under 1 s, inside `TimeoutStopSec`).
- Every configured sensor appears in every sweep. A missing one is
  `"temp_c": null` with `"error": "not_present"`, never left out.
- The timestamp is when the sweep actually ran, in UTC RFC 3339, with
  `clock_synced` taken from `/run/systemd/timesync/synchronized`.
- Calibration: `temp_c = raw_c + offset_c`. `offset_c` is optional and
  defaults to 0. Both are published, so a consumer can see the raw value, and
  a sensor with no offset is visibly uncalibrated. Two-point calibration (gain
  as well) is left out until bath data shows it's needed. If it ever is, it's
  an additive config change, recording the measured points rather than a
  gain worked out by hand.

Error codes are stable strings, and part of the wire contract: `not_present`,
`bus_low`, `crc`, `power_on_reset`, `bad_scratchpad`, `out_of_range`,
`malformed`, `read_error`.

### Wire contract (draft; normative copy goes in docs/WIRE.md)

All payloads are UTF-8 JSON. Unknown values are `null`, never 0.

| Topic | When | QoS | Retained |
|---|---|---|---|
| `energy/hot_water/{device_id}/state` | every sweep | 0 | yes |
| `energy/hot_water/status` | STARTUP, ready, HEARTBEAT, SHUTDOWN; LWT | 1 | yes |

The prefix is configurable, and `energy/hot_water` is the default. The status
topic belongs to the daemon, not to a tank. One Pi per site is assumed: a
second tanktemp at the same site would need a different prefix.

**State**, one message per tank per sweep:

```json
{"device_id":"hot-water-cylinder","timestamp":"2026-11-02T21:06:26Z",
 "clock_synced":true,"sweep_ms":3988,
 "sensors":[
  {"position":"top","order":1,"height_mm":1500,"rom":"28-0000117287e6",
   "temp_c":61.19,"raw_c":61.375,"offset_c":-0.19,
   "error":null,"suspect":null,"attempts":1,"bus":"w1_bus_master2"},
  {"position":"bottom","order":5,"height_mm":100,"rom":"28-000011721ce9",
   "temp_c":null,"raw_c":null,"offset_c":0.05,
   "error":"not_present","suspect":null,"attempts":1,"bus":null}]}
```

Sensors are in `order`, and the order of keys is fixed (it is pinned by a
golden test). A tank with no valid reading still publishes. Asset facts
(capacity, model and so on) are **not** in the payload. They live in
`devices_<site>`, and statehouse joins them on.

**Status**: the `{"online":…}` shape that ups-mqtt, netmon and tempest-mqtt
already use. It becomes `mqttpub`'s convention:

```json
{"online":true,"event":"HEARTBEAT","timestamp":"…","version":"<commit>",
 "ready":true,"uptime_s":86400,"tanks":["hot-water-cylinder"],
 "sensors_ok":9,"sensors_failed":1,"unexpected":["28-000011721aa5"],
 "startup_problems":[{"rom":"28-…","code":"ext_power_parasite"}]}
```

The LWT is `{"online":false,"event":"LWT"}`. SHUTDOWN carries `"reason"`.
`ready` becomes true after the first complete sweep.

Retained state means a consumer that connects late still gets the last
reading. Its `timestamp` is what tells it whether that reading is stale.

### /healthz and the status page

`/healthz` comes from `common/health`. It **always returns 200**, and the
aggregated status is in the body:
- `starting`: no complete sweep yet;
- `degraded`, with a `detail` line for each cause: MQTT disconnected; any
  configured sensor failing in the last sweep; the last sweep older than 3 ×
  the interval; the clock not synced; any startup problems;
- `ok`: none of the above.

It reports the same fields as boiler-sensor and pi-price-screen: `status`,
`detail`, `version`, `uptime_s`, `start_time`, `goroutines`, `heap_mb`,
`sys_mb`. It adds a `system` block from `health/sysinfo` (CPU temperature,
load average, memory, root disk, the `NETWORK_*` values from
`/run/pi-helper.env`), and a `tanks` summary. boiler-sensor's `deploy.sh`
checks `version` and `status`, so tanktemp's deploy can use the same check.

`/api/status` returns the full last sweep and startup report as JSON. `/` is a
small server-rendered page with one column per tank, sensors from top to
bottom, failures shown in place. It fetches `/api/status` every interval.
Unlike boiler's page, it doesn't use MQTT websockets, so it has no broker
dependency.

### Testing (red/green throughout)

For each behaviour: write the failing test, run it and record the red, then
write the code. Subagents report the red as well as the green.

There are two layers, each with its own job.

#### Layer 1: core logic, with everything external faked

`sweep`, `payload`, `startup`, `status`, `config`, and `main`'s wiring are
tested with **no filesystem, no sysfs and no broker**. The boundary test
above guarantees it.

- **`readingtest.Fake`** is a scriptable `reading.Source`:
  - a topology (which ROMs are on which bus, plus unexpected ones);
  - a queue of Readings per ROM, one per sweep, so a test can script "OK,
    OK, not_present, not_present, OK";
  - an optional per-read latency, to drive parallel and cancellation timing;
  - a call log, so a test can assert that it asked for exactly the
    configured ROMs.
- **The scenarios are business rules.** Every configured sensor appears in
  every sweep. Calibration. Ordering. A tank whose sensors all fail still
  publishes. Unexpected ROMs. Health moving to `degraded` and back on fault
  and recovery. A stale sweep. An unsynced clock. Fault sequences taken from
  `prototype/samples` (power loss, data line, rewire), scripted as Reading
  sequences.
- **`mqttpub.Fake`** (`Find`/`IndexOf`/`Retained`/`All`/`Reset`) for
  publishing, and a fixed clock.
- **Golden wire tests:** `internal/payload/testdata/*.golden` pin the state,
  status and LWT bytes. A diff is a regression; the literals are never edited
  to make a test pass. CI runs them as their own step, as boiler-sensor's does.
- **Lifecycle wiring:** one test in `cmd/tanktemp` drives the real
  `lifecycle.Run` with the fake Source, the fake publisher and a fake clock.
  STARTUP → ready → HEARTBEAT → SIGTERM mid-sweep (a latency-scripted Fake)
  → SHUTDOWN. It pins the bytes and checks that shutdown is prompt.
- **Hand-written fakes, not mockgen.** The fakes have to play back sequences
  and timing. Expectation-style mocks would pin call order rather than
  behaviour, and break on every refactor. This matches boiler-sensor and
  ups-mqtt.

#### Layer 2: the 1-Wire handler, against real fixtures

`internal/onewire` is the only code that touches the hardware, so it's where
the device's quirks are pinned down.

- **Real sysfs trees as fixtures.** `cmd/w1capture` copies the files that
  matter (`w1_bus_master*/w1_master_slaves`, and for every device `w1_slave`,
  `ext_power`, `resolution`) from the Pi into
  `internal/onewire/testdata/sysfs/<scenario>/`. A sequence is captured as
  `step-001/`, `step-002/`, …, and replayed by a sequence FS, one step per
  read. It is read-only, so it's safe on the rig at any time. Tests open
  fixtures with `os.DirFS`, so they're real files in the real layout.
- **One table test over every scenario directory.** Each has an
  `expect.json`: the Readings, Topology and Info that `onewire.Source` must
  produce. **Adding a fixture is adding a test**: capture it, write its
  `expect.json`, see it fail (red), then fix the handler.
- **Scenarios:**
  - Captured from the rig now, read-only: 10 sensors on two buses; the
    current bus numbering; `ext_power` and `resolution` for each sensor.
  - Captured with your hands on the hardware: a probe unplugged (ENOENT, then
    dropped from the bus); the bus numbers swapped by a reboot; a power cycle
    of one sensor (the real power-on-reset scratchpad, which settles the
    85 °C question); a data line pulled low.
  - Until those exist, some are **synthetic**, built from the byte patterns in
    ONEWIRE.md: power-on reset, bus held low, kernel `crc=… NO`, all-`ff`,
    junk `00-…` devices, `not found.` on an empty bus. Each is marked
    `"synthetic": true` in its `expect.json`, and replaced by a real capture
    when one exists.
  - Kept from the prototype: `TestRealCaptures` (all 10 real scratchpads),
    the CRC and resolution tables, and `seqFS` recovery after a reset.
- **Fuzzing:** `FuzzParseW1Slave`, `FuzzScratchpadCheck` and
  `FuzzMasterSlaves`, seeded from every fixture. The invariants:
  - never panic;
  - every error wraps a known sentinel;
  - when `Check()` passes, the CRC is valid, the value is in range, and it
    matches the kernel's `t=` value.

  CI runs the seeds on every push, plus a short `-fuzz` run. Longer runs are
  done locally.
- **Exhaustive where it's cheap:** all 65,536 raw temperature register values
  go through `MilliC` and are compared with the kernel's formula.
- **Concurrency:** `-race`, parallel reads per bus with injected latency, and
  `ctx` cancelled mid-sweep, which must return within one read per bus.
- **The contract suite:** `readingtest.Contract(t, newSource)` states what
  every `reading.Source` must do: every requested ROM comes back; a code is
  never `OK` without a value; a missing ROM is `NotPresent` in one attempt;
  cancellation is honoured. It runs against **both** `readingtest.Fake` and
  `onewire.Source` over the fixtures, so the fake used in layer 1 can't drift
  from the real handler.
- **Hardware check (`//go:build hardware`), never in CI:** a cross-compiled
  test binary run on the rig, read-only. Every configured ROM is present,
  passes its checks, is in powered mode and reads at 12-bit. It is run by a
  script with `--check` semantics, as your CLAUDE.md requires.
- **Extensible, without speculating:** a family seam keyed on the ROM prefix
  (`28` → the DS18B20 decoder). Only DS18B20 is implemented, and a config
  naming any other family is refused. A new family (or `therm_bulk_read` as
  a second read strategy behind the same `Source`) means a decoder, its
  fixtures, and the contract suite passing. Nothing outside `onewire`
  changes.

#### Everything else

- **Integration (`//go:build integration`):** the real publisher against
  Mosquitto through `mqttpub`'s fault-injecting proxy (from ups-mqtt's
  `mqtttest`).
- **CI:** `go vet`, `go test -race -cover`, the boundary test, the golden
  step, a short fuzz run, cross-builds (linux/arm GOARM=6, GOARM=7, arm64),
  and shellcheck plus `deploy_test.sh`. The `onewire`, `reading`, `config`,
  `sweep` and `payload` packages target 100% coverage.

## The common modules

These follow the pattern of `remoteconfig` and `devices`. First a design doc
in common, shown to the user before any code. Then the module is built.
Consumers move one at a time, through a gitignored `go.work`, and each is
deployed and verified before the next. The module is tagged only once a real
consumer has run on it. Never put a `go.work` at `~/src/github.com/sweeney/`.

### `github.com/sweeney/common/lifecycle` (stdlib only)

Today `iot.Run` (`iot/runner.go:58-245`) mixes the lifecycle with GPIO work.
This module holds the lifecycle, and knows nothing about MQTT, GPIO or
payload shapes:

```go
type Kind int // Startup, Ready, Heartbeat, Shutdown

type Event struct {
	Kind   Kind
	At     time.Time
	Reason string // Shutdown only: "SIGTERM", "SIGINT", …
}

type Config struct {
	Work      func(ctx context.Context, now time.Time) error // one unit of work; may block
	Ready     func() bool                                    // first Ready event once true
	Emit      func(Event) error                              // the caller formats and publishes
	Interval  time.Duration                                  // work cadence (iot: 100ms poll; tanktemp: 30s)
	Heartbeat time.Duration

	Now     func() time.Time      // injection points, defaulting to production
	Tick    <-chan time.Time
	Signals <-chan os.Signal
	Logger  *log.Logger
}

func Run(ctx context.Context, cfg Config) error
```

Behaviour:
- Emit Startup; then on each tick run `Work` in its own goroutine. A tick
  that arrives while work is still running is skipped and counted, never
  queued.
- Emit Ready once, at the first tick after `Ready()` is true.
- Emit Heartbeat on schedule.
- On a signal or a cancelled `ctx`: cancel the work's context, wait for it
  to return, then emit Shutdown with the reason.
- Signals are always handled at once, even mid-sweep. This is the reason
  for the module: it fixes the blocking concern in ONEWIRE.md for tanktemp,
  and costs boiler nothing.

**The main risk is boiler-sensor's bytes and timing.** Today the heartbeat
schedule, and the uptime and counts in its log line, come from
`logic.Detector.CheckHeartbeat`. The design doc has to say how `iot` keeps
that: either `iot` supplies a `HeartbeatDue` hook, or `lifecycle` owns the
schedule and `iot` proves it fires on the same ticks. That is settled by
tests, not by argument. `iot`'s `runner_test.go` and `wire_contract_test.go`,
plus boiler-sensor's `lifecycle_golden_test.go`, must pass **unchanged**
against the rebuilt `iot.Run`.

**Consumers and tags:**
1. `iot` is rebuilt on it. `iot`'s tests and boiler-sensor's goldens pass
   unchanged through `go.work`.
2. tanktemp runs on it on the Pi.
3. Then `lifecycle/v0.1.0` and `iot/v0.2.0` are tagged. `iot/v0.1.0` was
   likewise tagged before boiler-sensor shipped on it.
4. boiler-sensor bumps and deploys in its own session (sweeney/boiler-sensor#6,
   part 1), verifying `/healthz`, the retained HEARTBEAT and statehouse CH/HW.

Later candidates: ups-mqtt and netmon, whose daemons have the same shape.

### `github.com/sweeney/common/health` (stdlib only)

- `health.Report`: the shared fields (`status`, `detail`, `version`,
  `uptime_s`, `start_time`, `goroutines`, `heap_mb`, `sys_mb`) in a fixed
  order, plus service-specific fields that follow them.
- `health.Handler(func() Report) http.Handler` always returns 200 with
  `Cache-Control: no-store`. Statuses are `ok`/`starting`/`degraded`, as
  constants.
- `health/sysinfo`: `Read(fsys fs.FS) Info`, covering
  `/sys/class/thermal/thermal_zone0/temp`, `/proc/loadavg`, `/proc/meminfo`,
  `/proc/uptime`, and a root-disk `Statfs` behind an interface. It also has
  `PiHelperEnv(path)`, the `NETWORK_*` parser that `common/iot/runner.go`
  and netmon both carry today. Only files seen on the Pis in use are read,
  with nothing speculative. Before it is written, each path is checked on a
  real Pi Zero 2 W and on a Pi Zero.
- **Consumers and tag:** tanktemp first, through `go.work`. It is tagged
  `health/v0.1.0` once tanktemp has run on it on the Pi. Then boiler-sensor
  (sweeney/boiler-sensor#6, part 2) and pi-price-screen
  (sweeney/pi-price-screen#3), each in its own session. Before each existing
  consumer migrates, it writes a golden test of its current `/healthz` body.
  Afterwards the golden compares **keys and values**, not bytes: key order
  changes in both, since boiler's own fields sit in the middle today and
  pi-price-screen's body is a map, written alphabetically. Both deploy scripts
  read `version`/`status` by key, so a reorder is safe for them.

### `github.com/sweeney/common/mqttpub` (Paho only)

Extracted from ups-mqtt's `internal/publisher`, `internal/retry` and
`internal/mqtttest`. ups-mqtt's copy is the source, and netmon's is a declared
copy of it. Wherever possible the history comes along via `git filter-repo`,
as the identity move did.

- `Message{Topic, Payload, QoS, Retained}`, `Publisher{Publish, Close}`.
- `MQTTPublisher` with `Tuning`/`DefaultTuning`, Paho auto-reconnect **off**
  (ups-mqtt's CLAUDE.md gives the reason), its own capped-backoff reconnect
  loop, an LWT, a `Reconnected()` channel, and `Connected()` for `/healthz`.
  netmon's `Watch` (subscribe) comes along too, since netmon is a consumer.
- `retry.Backoff` / `retry.Connect`.
- `Fake` with `Find`, `IndexOf`, `Retained`, `All`, `Reset`.
- `mqtttest`: the Mosquitto bridge plus the fault-injecting proxy.
- **Not in it:** topics or payload shapes. The only shared shape is the
  `{"online":false}` LWT, and that is a helper the caller can choose not to use.
- **Consumers, in order:** ups-mqtt, deployed and verified; then netmon; then
  tanktemp. The tag waits until ups-mqtt has run on it. Each migration keeps
  that repo's existing payload tests unchanged.
- tempest-mqtt, asterisk-mqtt and power-monitor have simpler publishers, with
  auto-reconnect on. They are listed in common's PLAN.md as possible later
  consumers. They are not moved now.

### `common/devices` follow-up: assets, cylinders, `part_of`

This is a minor version after `devices/v0.1.0`. PR #7 merges and is tagged
first, unchanged. It needs its own section in `devices-DESIGN.md`, reviewed
before any code. The new keys go into the namespace only once statehouse has
shipped reading them (the devices versioning rule).

**Generic, on every `Device`:**

| Key | Type | Notes |
|---|---|---|
| `manufacturer` | string | |
| `model` | string | |
| `installed` | string, `YYYY-MM-DD` | a civil date with no time zone; a bad value is `device.bad_value` |
| `part_of` | string, a device id | see below |
| `telemetry` | bool, absent = `true` | `false`: the entry describes a thing that sends no readings itself (a cylinder, a fridge). A `*bool`, so absent and `false` stay distinct |

**`cylinder`**, a typed block like `thresholds`, for physical facts. All of
them are declared, never computed from each other:

| Key | Type | Notes |
|---|---|---|
| `capacity_l` | number | the nominal capacity on the label |
| `height_mm` | number | external |
| `diameter_mm` | number | external |

Left out for now: `heat_sources` (coil or immersion, kW), `heat_loss_kwh_24h`,
serial number, maximum working pressure, and the last service date. Each is a
minor version later, if a reader and the facts turn up.

These are pointers, so an explicit 0 is honoured and an absent key isn't
serialised as zero. Decoding is per field, as for every other field: a bad
value loses only that key.

**`part_of`: one model for anything with probes inside it.** The cylinder
and the fridges and freezers are the same case: a thing, and probes inside it
that are charted but never averaged with room air.

- **The thing** is a device entry with a class (e.g. `hot_water_cylinder`,
  `fridge`, `freezer`), `telemetry: false`, its asset fields, a room and a
  floor. It has no `scheme`/`primary`.
- **Each probe** is an entry with its own identity (`scheme=tanktemp`,
  `primary=<device_id>.<position>`; or the fridge probes' existing
  identities), class `appliance_probe`, and `part_of: <thing id>`.
  `target_temperature` stays on the probe, because it is what the probe
  should read. Cylinder probes may have one too, if you want it.
- **`Place()` resolves one level:** a device's own room if set, else its
  `part_of`'s room. Because it is in the shared module, statehouse,
  countinghouse and greenhouse can't place a probe differently.
- **`Check`** gains `device.part_of_unresolved` (it points at nothing),
  `device.part_of_chain` (it points at something that is itself `part_of`,
  or a cycle), and `device.part_of_room_mismatch` (both declare a room and
  they differ).
- **`telemetry: false`** (decided 2026-10-31) is how `Check` knows the thing
  has no identity on purpose. It is declared on the entry, never inferred
  from what points at it. So:
  - `device.identity_missing` is skipped for `telemetry: false`, and only
    for that. A thing added before its probes doesn't warn. A real device
    with a typo in its identity still warns, even when something is
    `part_of` it.
  - **New code `device.telemetry_contradiction`:** an entry with
    `telemetry: false` that also has a `scheme`/`primary` or `ieee_address`.
  - Forgetting the key gives the existing `identity_missing` warning, so the
    mistake is visible.
  - `Device` gains `SendsTelemetry() bool`: true unless the key is
    explicitly `false`. Consumers use it to leave these entries out of
    anything about readings or liveness (device lists, "never heard from"
    alerts, online counts) without each writing their own rule.
  - The key goes into the namespace only after the services that read it
    have shipped, as the versioning rule requires. Until then a
    `telemetry: false` entry decodes as an unknown key and still warns.
- **No migration is forced.** Today's fridge and freezer probes keep their
  own `room` and work unchanged. Adding an appliance entry and `part_of` is
  an edit per device, whenever you want it. The fridge's smart plug could be
  `part_of` it too.

**To verify first:** how statehouse, countinghouse and greenhouse treat a
configured device they never hear from today. If any of them lists it, counts
it as offline or alerts on it, the follow-up must make that service skip
`SendsTelemetry() == false`, in the same release as the key. Otherwise the
cylinder shows up as a dead device. The answer goes into the design section
before any code.

Example entries (made up):

```json
{
  "hot-water-cylinder": {
    "class": "hot_water_cylinder", "display_name": "Hot water cylinder",
    "telemetry": false, "room": "first.cupboard", "floor": "first",
    "manufacturer": "Example Co", "model": "UV-210", "installed": "2019-05-14",
    "cylinder": {"capacity_l": 210, "height_mm": 1480, "diameter_mm": 550}
  },
  "hot-water-cylinder-top": {
    "class": "appliance_probe", "display_name": "Cylinder: top",
    "scheme": "tanktemp", "primary": "hot-water-cylinder.top",
    "environment_fields": ["temperature_c"], "part_of": "hot-water-cylinder"
  }
}
```

Fixtures in common are made up, since the repo is public. Real makes, models
and install dates go only into the config namespace.

## Phases

Each phase ends green, with its docs updated. Before every commit I ask what
date and time it should carry, and grep the diff and message for real site,
host or namespace names (sweeney/common is public).

0. **Repo.** `git init`; the boundary test (red until the packages exist);
   move the prototype's `onewire` to `internal/onewire` with its tests; move ONEWIRE.md to `docs/` and the samples to
   `testdata`/`docs`; add CI. **Local only** until there's a good enough
   version. Then `sweeney/tanktemp` is created as a **public** repo and
   pushed, after a PII pass over the whole history (samples, docs, commit
   messages), and only when you say so.
1. **`config`:** the validator, written vector by vector, with a failing case
   and a near miss for each rule.
2. **`reading` + `onewire`:**
   - `reading` types, `readingtest.Fake` and the contract suite first.
   - Then `cmd/w1capture`, and the first read-only captures from the rig.
   - Then `onewire.Source` against the scenario table: context cancellation,
     the 85 °C retry and suspect flag, `ext_power`/`resolution`.
   - Fuzz targets.
   - I'll ask when I need your hands on the hardware for the unplug and
     power-cycle captures.
3. **`sweep` + `startup`:** calibration, error codes, ordering, unknown IDs,
   clock sync, fault replays.
4. **Wire contract:** write `docs/WIRE.md` and the goldens, and **show them to
   you before the encoders are written.** Then `payload`.
5. **common `lifecycle`:** design doc, then the module. Then `iot.Run` is
   rebuilt on it, with every `iot` test and boiler-sensor's goldens unchanged
   (through `go.work`). Tags come after phase 11.
6. **common `health`:** design doc, then module, then tanktemp's `/healthz`.
   Tag after phase 11.
7. **common `mqttpub`:** design doc, then extraction from ups-mqtt, then
   ups-mqtt deployed, then netmon deployed, then the tag. tanktemp uses it
   through `go.work` meanwhile.
8. **`web` + `main`:** the wiring test, then `main.go`, then the page.
9. **Deploy:** `deploy/lib.sh` copied from boiler-sensor (`--check`,
   `--rollback`, `--yes`, arch from `uname -m`, version stamping, verification
   of `/healthz` and the retained status, automatic rollback, exit codes
   0/1/2/3). `setup-pi.sh` is idempotent, has `--check`, and checks or adds
   the `dtoverlay=w1-gpio` lines with a prompt before a reboot. Its
   `-check-config`, `deploy_test.sh` and the unit file follow boiler's
   hardening, without `gpio` and with read access to `/sys`. All of these are
   tested before you're asked to run them.
10. **common `devices` follow-up:** the design section, then the code, after
    `devices/v0.1.0` is tagged. It can run alongside phases 5–9.
11. **On the rig (the production Pi):**
    - `setup-pi.sh --check` and `deploy.sh --check`, read-only.
    - Note `templog`'s state, stop it (asking first), and leave a one-line
      way to bring it back.
    - Power-cycle one sensor and capture `w1_slave` to confirm byte 6 =
      0x0C. Then drop the interim 85 °C flag; TDD: the capture becomes a
      fixture first.
    - First deploy, then verify `/healthz`, the retained status and the
      state topics.
    - Then push `lifecycle/v0.1.0`, `iot/v0.2.0`, `health/v0.1.0` (and
      `mqttpub` once ups-mqtt has run on it), and comment on
      sweeney/boiler-sensor#6 and sweeney/pi-price-screen#3.
    - Calibration stays at offset 0.
12. **Hand-off:** `docs/STATEHOUSE.md` (below). common's PLAN.md records the
    new modules, the `iot` middle-path follow-up and the copied deploy
    `lib.sh`.

## docs/STATEHOUSE.md: what it will specify

This is written for statehouse to build its own `tanktemp` adapter, following
the pattern of its `boiler`, `ups` and `climate` adapters.

- Subscriptions: `energy/hot_water/+/state` and `energy/hot_water/status`. The
  base topic is configurable, as for the other adapters.
- **Identity:** one engine device per sensor, `scheme=tanktemp`,
  `primary=<device_id>.<position>`. A tank is not one engine device: each probe
  is its own `temperature_c` series in `device_environment`. The ROM is not
  the identity, because replacing a probe changes its ROM but not its
  position. The ROM is carried as metadata so a swap is visible.
- **The `devices_<site>` entries:** the cylinder entry (class,
  `telemetry: false`, room, floor, asset fields, `cylinder` block) and one probe entry per position, with
  `part_of` the cylinder. Example entries are made up.
- **Class: `appliance_probe`** (decided; docs widened in sweeney/statehouse#79
  and sweeney/greenhouse#45). greenhouse already charts that class and never
  averages it with ambient air. `part_of` names the cylinder, which is what
  tells a cylinder probe from a fridge probe.
- **A probe with no entry** resolves to `unclassified` (or a name hint) in
  statehouse (`internal/device/profiles.go:173-210`), and greenhouse doesn't
  chart it at all. So the entries go into `devices_<site>` before the first
  deploy, or the probes don't show up anywhere.
- **`target_temperature` is optional** on cylinder probes. If set, it is
  relayed raw like the fridge and freezer targets. What counts as too cold is
  the consumer's policy.
- **Mapping:** `temp_c` is written as `temperature_c`. A `null` writes
  nothing, and the adapter logs or counts the `error` code. `raw_c`,
  `offset_c`, `suspect`, `attempts` and `bus` are publisher metadata; the
  doc says which ones statehouse may want to keep.
- **Asset relay:** statehouse's device API shows the cylinder's
  `manufacturer`, `model`, `installed` and `cylinder` block as they are. They
  are config facts relayed raw: no computed kWh and no "full" verdict.
- **Online and offline:** what `online:false` (LWT or SHUTDOWN) should do to
  the devices, how to treat a retained state message whose `timestamp` is old,
  and `clock_synced:false`.
- **Compatibility:** the wire goldens are the contract. A new field is
  additive, and removing or renaming one is a major change announced in that
  doc first.
- A worked example: a recorded sweep sequence covering a fault (from the
  data-line fault replay), with the engine updates each message should cause.

## Open questions

None open as of 2026-10-31. Questions that turn up during the build go here.

## Build status (2026-10-31, overnight)

Everything is local. Nothing is pushed, tagged or merged.

| Phase | State |
|---|---|
| 0 Repo | done |
| 1 config | done |
| 2 reading + onewire | done. Real rig captures plus 12 synthetic faults; fuzzing; the contract suite runs against both the fake and the real handler |
| 3 sweep + startup | done |
| 4 wire contract | done, **not reviewed by you** (GUESSES G13). `docs/WIRE.md`, goldens |
| 5 lifecycle | built in common. **`iot` is not rebuilt on it**: two test incompatibilities, see `lifecycle-DESIGN.md` in common |
| 6 health | built in common, with `sysinfo` |
| 7 mqttpub | built in common (extracted from ups-mqtt/netmon by an agent, plus a lock on the Fake). **ups-mqtt and netmon are not migrated** |
| 8 web + main | done; run end to end on the Mac against Mosquitto, including the last will |
| 9 deploy | done: `setup-pi.sh`, `deploy.sh`, 267 stub tests |
| 10 devices follow-up | built in common: asset fields, `cylinder`, `part_of`, `telemetry` |
| 11 the rig | **running on the rig Pi**, publishing under `test/energy/hot_water`, with `templog` stopped. The hardware check passes. Not done: real power-on-reset and data-line captures (they need your hands), and the water-bath calibration |
| follow-up (2026-11-01) | **Remove templog** (G24) once tanktemp has run for a week, from 2026-11-08 at the earliest. Until then it stays disabled, as the fallback |
| 12 hand-off | `docs/STATEHOUSE.md` written |

Released on 2026-11-01. The three branches were merged into
`sweeney/common` (#8 `lifecycle` and `health`, #9 `mqttpub`, #10 `devices`)
and tagged `lifecycle/v0.1.0`, `health/v0.1.0`, `mqttpub/v0.1.0` and
`devices/v0.2.0`. tanktemp pins the first three in `go.mod` and no longer
uses a `go.work`. tanktemp is public at `sweeney/tanktemp`.
