# tanktemp

Reads DS18B20 1-Wire temperature probes fitted to unvented hot water
cylinders, on a Raspberry Pi. It publishes every sweep to MQTT and serves
`/healthz` and a status page. Any number of cylinders, and any number of
probes on each, is set in a local config file. It also counts a reed-switch
hot-water meter on a GPIO line, one litre per pulse, and publishes the count
([Water meter](#water-meter)).

- **Wire contract:** [`docs/WIRE.md`](docs/WIRE.md), pinned by the goldens
  in `internal/payload/testdata/`.
- **For statehouse:** [`docs/STATEHOUSE.md`](docs/STATEHOUSE.md), the
  adapter spec.
- **The hardware, and everything learned on the test rig:**
  [`docs/ONEWIRE.md`](docs/ONEWIRE.md) for the probes,
  [`docs/METER.md`](docs/METER.md) for the water meter.
- **The water meter's plan**, including what washhouse does with it:
  [`docs/WATER-METER-PLAN.md`](docs/WATER-METER-PLAN.md), with a worked day
  in [`docs/WATER-METER-WALKTHROUGH.md`](docs/WATER-METER-WALKTHROUGH.md).
- **Plan and decisions:** [`PLAN.md`](PLAN.md). Choices made without asking
  are in [`GUESSES.md`](GUESSES.md).

## How it works

```
/sys/bus/w1/devices ──> internal/onewire ──reading.Source──> internal/sweep ──> internal/payload ──> MQTT
   (the only code that                    (the boundary)     (calibrate,        (wire contract)
    knows about sysfs)                                        order, quality)
                                                               │
                                                               ├──> internal/status ──> internal/web (/healthz, /api/status, /)
                                                               └──> internal/cadence ──> how long until the next sweep

/dev/gpiochip0 ──> internal/gpioedge ──meter.Source──> internal/meter ──> internal/payload ──> MQTT ({prefix}/meter)
   (the only code that                 (the boundary)   (Counter: the    (wire contract)
    knows about GPIO)                                    litre rules;
                                                         Loop: owns it)
```

The meter runs beside the sweeps, on its own goroutine, so a litre never
waits for a 1-Wire read. Each sweep also publishes the meter's count as of
its start, so the tank states and the count are a matched snapshot.

`cmd/tanktemp` wires these together with
[`common/lifecycle`](https://github.com/sweeney/common) (signals, STARTUP,
heartbeats, SHUTDOWN, cancellable sweeps), `common/mqttpub` (the resilient
publisher) and `common/health` (`/healthz`, `sysinfo`). lifecycle's
`Next` hook asks when each next sweep is due, and `internal/cadence`
decides (below).

## Adaptive cadence (sweep fast while probes are changing)

A hot-water draw shows within a sweep or two: the cold inlet falls and the
outlet rises. Its volume comes from the energy balance, which doesn't need
fast sampling, but its start, end and flow rate do. Sweeping fast all the
time triples the messages, nearly all of them overnight cooling. So, with
`sweep.fast` in the config (issue #1), tanktemp sweeps slowly by default
and speeds up only while a probe is changing:

```yaml
sweep:
  interval: 30s              # slow: the longest gap between sweeps
  fast:                      # optional; absent = a fixed interval
    interval: 10s            # jump straight to this...
    trigger_c_per_min: 1.5   # ...when any probe changes at least this fast, either way
    hold: 2m                 # stay at fast.interval this long after the last trigger...
    decay: 5m                # ...then ease back to interval over this long
```

```
interval
 30 s ─────┐                                   ╭──────────
           │                              ╭────╯
           │                         ╭────╯      fast × (slow/fast)^((t−hold)/decay)
 10 s      └────────────────────╭────╯
           ▲ trigger    hold    ▲      decay   ▲
           t = 0               2m              7m
```

- **The trigger is a rate**, between a probe's reading in one sweep and the
  next, so it means the same at either interval. The change must also be
  at least 0.125 °C (two sensor steps), so a one-step flicker never
  triggers. Only two OK readings in a row count: a failed probe, one that
  just recovered, or a suspect 85 °C can't trigger.
- **The speed-up is immediate, the slow-down gradual.** The next sweep is
  `fast.interval` after the one that saw the change. Any new change jumps
  straight back to fast. With the numbers above, one event gets 33 sweeps
  in the 8 minutes after it, where a fixed 30 s gives 16.
- **Nothing on the wire changes.** Consumers see state messages at least
  every `interval`, and more often while probes change; `timestamp` stays
  authoritative, and staleness is still 3 × the slow interval
  (`docs/WIRE.md`, Cadence).
- **`/api/status` shows it**, for tuning, in a `sweep` block: `adaptive`
  (whether `sweep.fast` is set), `mode` (`slow`, `hold`, `decay`),
  `interval_s`, `next_due` and `next_in_s` (from the Pi's own clock;
  negative while a sweep is running), `last_trigger` (probe, ROM and
  signed rate) and `fast_entries`. The status page counts down to the next
  sweep, polls again just after it, and backs off while one is overdue.
  With `log.verbose` on, every trigger and change of mode is logged
  (below). `-check-config` says what the cadence is.

Every key in `fast` has the default shown, so `fast: {}` turns it on. A
`fast:` with nothing under it (every key commented out) is refused rather
than read as "off": leave the block out for a fixed interval.

### How it works

- **`internal/cadence` decides; it's pure.** After each sweep,
  `Cadence.Observe` compares every probe with the sweep before and returns
  the interval to the next one. The interval is a function only of the
  time since the last trigger, so the whole curve is table-tested with no
  clock at all.
- **It runs on the monotonic clock.** Rates and the curve use each sweep's
  start as `common/lifecycle` hands it to the work (`time.Now`, monotonic).
  The Pi has no real-time clock and NTP can step the wall clock at boot;
  that step can't fake or hide a trigger. The published `timestamp` is
  still the wall clock.
- **lifecycle schedules it.** lifecycle's `Config.Next` hook (added for
  this, `lifecycle/v0.2.0`) asks for the next sweep's time only once it has
  seen the last one finish, so a sweep can never be due while one is still
  running. `cadence.Schedule` keeps the due times on a grid (each one
  interval after the previous due time), so scheduling latency can't
  accumulate. A sweep that overruns skips the due times it missed (counted
  in `/healthz`'s `skipped_ticks`); nothing is queued.
- **Staleness and health don't change.** They're measured in slow
  intervals, so a faster cadence can only make data fresher.

### Tuning

The default trigger (1.5 °C/min) and hold (2m) started as guesses from two
days of recordings (GUESSES G38, G39); tune them from `/api/status` and the
verbose log. A 23 h replay on the rig confirmed 1.5: it catches every draw a
lower trigger does, at most one sweep later, for about 7% fewer messages.
It leaves slow warming (under about 1.5 °C/min) at the slow interval; set
1.0 to sample reheats fast from their first minute. Above 2.0, reheats
start late and small tap-run draws are missed. One thing the first test on
the rig showed (G51): at a 10 s fast
interval the trigger is, in practice, a number of sensor steps. 1.0 °C/min
over 10 s is 0.167 °C, three DS18B20 steps, while over 30 s it's eight. So
staying fast is much easier than going fast. On the rig that was right
(after a reheat the outlet probe keeps moving for minutes), but on a noisy
probe a low trigger could hold fast mode indefinitely; 1.5 °C/min or more
at 10 s keeps it to four steps.

## Water meter

A reed-switch meter on the hot-water pipe closes a contact once per litre.
tanktemp counts the closings on one GPIO line and publishes raw facts to
`{prefix}/meter`, retained: the count, when the last litre came, how long the
reed stayed closed, and how many edges it rejected and why. Flow, draws and
circulation are for consumers. The payload is in
[`docs/WIRE.md`](docs/WIRE.md), Meter; the hardware, the kernel's behaviour
and what the rig showed are in [`docs/METER.md`](docs/METER.md).

```yaml
meters:                            # optional; absent = no meter, no GPIO opened
  - device_id: hot_water_meter     # its key in devices_<site>; unique across tanks too
    gpio: 27                       # the BCM line (2..27), wired to the reed with GND
    litres_per_pulse: 1            # the meter's K (at most 1000)
    # chip: gpiochip0
    # bias: pull-up                # pull-up, pull-down, or none (an external resistor)
    # closed_level: low            # what the line reads with the reed closed: low or high
    # debounce: 20ms               # kernel debounce: required, 5ms..100ms
    # hold: 20ms                   # a change counts once held this long (at most 100ms)
    # min_interval: 600ms          # the shortest gap between litres (at most 700ms)
```

The pin and the wiring are in the config, not the code: tanktemp requests
exactly the `chip` and `gpio` given, with the `bias` given, and reads
`closed_level` as closed. The defaults are a reed between the GPIO and
ground with the Pi's internal pull-up, so open reads high and closed low.
One meter is supported for now.

- **How a litre is counted.** One litre per change from open to closed,
  judged on the level the kernel reads, never on an edge's label. The
  kernel debounces first. Then, in order, an edge is rejected as `lost` (a
  gap in the kernel's event numbers: tanktemp resyncs and counts nothing
  across it), `noise` (it doesn't change the level), `short` (a change
  either way that doesn't hold for `hold`: a spike, through which a litre
  in progress carries on) or a `glitch` (too soon after the last litre).
  The counters are published, and expected to stay 0.
- **When it publishes.** A `pulse` within `hold` + 50 ms of each counted
  closing; an `open` within `hold` + 50 ms of the opening that ends it,
  with how long it was closed; and a `sweep` with every sweep, and a last
  one at shutdown, before SHUTDOWN, after which nothing more. `seq`
  numbers the messages with no gaps, and `count` never falls as `seq`
  rises.
- **Nothing is persisted.** The count starts at 0 when tanktemp starts, and
  `counting_since` says when; consumers take differences.
- **Health.** `/healthz` and `/api/status` have a `meters` block: `state`
  (`starting`, `counting`, `failed`, `stopped`), the level, the count and
  counters, `publish_dropped` and `queued`. The status message lists
  `meters`. A line that can't be requested (busy, not permitted, no chip)
  is the startup problem `gpio_request_failed`, and a line that stops
  answering while counting (its chip gone) is `stopped`, with the reason:
  either way tanktemp is `degraded`, and the probes carry on. The status
  page doesn't show the meter yet.
- **`-check-config`** names the meter, its line, its wiring and its
  timings.
- **Permissions.** The unit gives tanktemp the GPIO chips and the `gpio`
  group, and nothing else new ([Deploying](#deploying)).

To watch the line on the Pi, with tanktemp stopped (only one process can
hold it), `cmd/pulsewatch` runs the same loop and counter as tanktemp and
prints what they make of each edge. It takes the line and every setting
from tanktemp's config; a flag overrides one:

```sh
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -o pulsewatch ./cmd/pulsewatch
# copy to the Pi, stop tanktemp, then:
./pulsewatch -config config.yaml -for 2m           # the config's meter (-meter to pick one)
./pulsewatch -config config.yaml -debounce 10ms    # tuning: one value changed
```

## Testing

```sh
go test -race ./...
```

No hardware, root or broker needed. There are two layers:

- **Core logic** (`cadence`, `config`, `meter`, `sweep`, `payload`,
  `startup`, `status`) sees the sensors only through `reading.Source` and
  the meter only through `meter.Source`. It's tested against the scriptable
  `readingtest.Fake` and `metertest.Fake`. The GPIO code itself
  (`internal/gpioedge`) is portable but for one request function, and
  meets `metertest.Contract` with a fake chip. `boundaries_test.go` fails the
  build if a core package imports `os`, `io/fs`, `net`, `syscall`, the
  1-Wire handler or MQTT.
- **The meter's counter** (`internal/meter`) is table-tested edge by edge,
  and replays a real night's raw edges from the rig
  (`internal/meter/testdata/first-night`: 277 edges, the boiler's spikes
  included) against an `expect.json` worked out independently. The replay
  runs raw, debounced, and with the state machine alone. `meter.Loop` is
  tested for ordering under `-race`: a tick or a snapshot never overtakes a
  queued edge. `metertest.Contract` runs against both the fake and the real
  GPIO source; `cmd/tanktemp` drives the whole meter path (messages, `seq`,
  the queue, SHUTDOWN, a failed request) with the fake.
- **The adaptive cadence end to end** (`cmd/tanktemp/scenario_test.go`) runs
  the real `run` on production timers inside a `testing/synctest` bubble,
  so time is virtual and exact. Each scenario is a table row: what each
  probe reads sweep by sweep, how long each sweep takes, what goes wrong
  when (a failed probe, a discovery error, a wall-clock step, SIGTERM), and
  what every gap between sweeps and the verbose log must show. A new corner
  case is a new row. The status page's polling rule runs under `node`
  (`internal/web/pagejs_test.go`); CI needs node, a developer's run skips
  without it.
- **The 1-Wire handler** (`internal/onewire`) is tested against real sysfs
  trees captured from the rig with `cmd/w1capture`, plus synthetic faults
  generated from them (`internal/onewire/testdata/gen_fixtures.py`). Every
  directory under `testdata/sysfs` is a test case with its own
  `expect.json`. There are fuzz targets, every raw register value is
  checked, and the reading contract suite runs against both the fake and the
  real handler. CI fuzzes each target for a fixed 1,000,000 inputs
  (GUESSES G52–G53); a new `Fuzz…` target must be added to the `fuzz
  (short)` step, and `ci_test.go` fails until it is. For a longer run
  locally, `go test -run '^$' -fuzz '^FuzzParseW1Slave$'
  -fuzztime 10m ./internal/onewire/`; a time-based run that ends with
  `context deadline exceeded` and no new file under `testdata/fuzz` found
  nothing.

To capture a new hardware state as a fixture (read-only on the Pi):

```sh
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -o w1capture ./cmd/w1capture   # the rig's Zero W; arm64 for a Zero 2 W
# copy to the Pi, then:  ./w1capture -out /tmp/<scenario> [-steps 6 -interval 10s]
```

### Fitting probes

`deploy/probes.sh` shows what's on the Pi's buses right now: each probe's
bus, temperature, and where the config puts it. It's read-only on the Pi.

```sh
deploy/probes.sh --host user@host            # the list
deploy/probes.sh --host user@host --watch    # a line as each probe arrives or leaves
deploy/probes_test.sh                        # the script's tests
```

To fit probes, disconnect them all, then connect them one or two at a time
with `--watch` running. Each arrival prints its ROM and temperature. A probe
appears up to ~10 s after it's connected, when the kernel next searches the
bus. When two arrive together, hold one in your hand: watch names the one
that warms by 0.5 °C. Note each probe's tank and height above the base as it
arrives; the config's `order` follows from the heights. While probes are
missing, tanktemp keeps running and reports them as `not_present`.

### Capturing a fault

`deploy/capture.sh` walks you through reproducing a fault on the rig and
capturing it as a fixture: it tells you what to do with your hands, captures
while you do it, copies the run back to
`internal/onewire/testdata/sysfs/rig-<scenario>-<date>/`, and says whether
the fault was caught. It's read-only on the Pi, and tanktemp keeps running.

```sh
deploy/capture.sh --check --host user@host power-on-reset   # preflight only
deploy/capture.sh --host user@host power-on-reset   # pull one probe's VDD for ~1 s, a few times
deploy/capture.sh --host user@host bus-low          # unplug one bus's pull-up for ~15 s
deploy/capture_test.sh                              # the script's tests
```

### Hardware check

A read-only test against the real sensors (build tag `hardware`, never run in
CI). It checks that every configured ROM is present, reads OK first time, is
in powered mode and is at 12-bit:

```sh
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go test -c -tags hardware -o onewire.test ./internal/onewire/
scp onewire.test user@host:/tmp/
ssh user@host 'TANKTEMP_ROMS=28-…,28-… /tmp/onewire.test -test.run Hardware -test.v; rm /tmp/onewire.test'
```

The meter's line has one too. It needs tanktemp stopped, because only one
process can hold the line. It requests the real line, runs
`metertest.Contract` against it, and checks the line can't be requested
twice. `TANKTEMP_METER_WATCH=60s` also prints every edge while you run a
tap ([`docs/METER.md`](docs/METER.md), Tools):

```sh
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go test -c -tags hardware -o gpioedge.test ./internal/gpioedge/
# on the Pi, with tanktemp stopped:
TANKTEMP_METER_LINE=gpiochip0:27 ./gpioedge.test -test.v
```

The common modules (`lifecycle`, `health`, `mqttpub`) are pinned in `go.mod`
to their tags in `sweeney/common`.

## Watching it live

`tools/live.html` is a single page that subscribes to `{prefix}/+/state` and
`{prefix}/status` over MQTT-over-WebSockets and draws each cylinder, the
publisher's status and the flow of messages. It only reads. The broker needs
a websockets listener (mosquitto: `listener 9001` with `protocol websockets`).

```sh
./tools/live.sh --check   # print the URL from deploy/config.local.yaml, probe the port
./tools/live.sh           # open it
./tools/live.sh --demo    # made-up data, no broker
./tools/live_test.sh      # the script's tests
```

## Verbose logging (at the bench)

tanktemp normally logs very little: startup problems, sweeps with faults,
MQTT reconnects, and start and stop. While wiring, testing or tuning, turn on
`log.verbose` in `deploy/config.local.yaml`:

```yaml
log:
  verbose: true
```

then `./setup-pi.sh --check` and `./setup-pi.sh` to install it and restart
(it's read only at start; a restart costs one sweep). The journal then also
gets:

- **every sweep, one line:** duration, probes OK/total, then each tank's
  calibrated readings to 0.1 °C from the bottom (order 1) up, tanks
  separated by `|`. A failed probe is `null` in its place, and the usual
  fault line follows with the same duration. A thermometer on a bus that
  isn't in the config ends the line:

  ```
  sweep 4.03s 9/10 left 36.2 58.4 null 66.1 65.2 | right 39.9 53.3 54.1 49.6 57.2 | unexpected 28-…
  sweep: 9 ok, 1 failed, 4.03s: crc: left/middle
  ```

- **every retried read:** the probe's tank/position and ROM, which attempt
  failed and why, and the extra read a 85.000 °C value gets (GUESSES G2),
  always with its outcome:

  ```
  read left/middle 28-… attempt 1/3: crc (scratchpad CRC mismatch)
  read left/top 28-… attempt 1/3: 85.000 °C, reading once more to check
  read left/top 28-… extra read: 60.000 °C, keeping it
  ```

Whether verbose is on or not, a failed discovery (no bus masters, say) leads
the fault line, because it's why every probe after it is `not_present`:
`sweep: 0 ok, 10 failed, 1ms: discovery failed: no 1-Wire bus masters;
not_present: …`.

`journalctl -u tanktemp -o cat | grep '^sweep '` gives the readings over
time. Verbose output never changes what is read, published or when: the lines
are built from the sweep already in hand.

`/api/status` reports `"verbose": true` and `-check-config` says
`verbose log ON` while it's on, so it's hard to leave on by accident. **Turn
it off after testing:** at a 10 s interval it's about 8,640 sweep lines a
day. journald copes, but they bury the lines that matter.

- **the adaptive cadence** (with `sweep.fast`): every trigger, with the
  probe and its rate, and every other change of mode, with the gap to the
  next sweep. They start `cadence:`, so `grep '^sweep '` still gives only
  the readings:

  ```
  cadence: fast 10s (left/bottom -3.2 °C/min)
  cadence: decay from 10.4s
  cadence: slow 30s
  ```

## Reviewing the guesses

`go run ./tools/guessreview -open` serves a local page (loopback only, port
8741) for answering `GUESSES.md` one guess at a time. Each answer is saved
as it's given to `tools/guessreview/answers.json`, which is gitignored.

## Deploying

```sh
./setup-pi.sh --check user@host   # read-only: what a first install would do
./setup-pi.sh user@host           # first install (full sudo): sudo rules, 1-Wire overlays from the config (+ reboot), config, unit, first build
./deploy.sh --check user@host     # read-only pre-flight
./deploy.sh user@host             # build, upload, switch, verify, roll back on failure
./deploy.sh --rollback user@host
```

tanktemp runs as the `tanktemp` system user (no home, no login), which
`setup-pi.sh` creates. Builds live in `/opt/tanktemp`, owned by the deploy
user with mode 0755. That directory holds the timestamped builds, the
`tanktemp-bin` link to the running one, the unit file, `.units/` and
`.previous`, and the unit runs `/opt/tanktemp/tanktemp-bin`.
`/etc/tanktemp/config.yaml` (and any `password_file`) is root:tanktemp 0640,
so `deploy.sh` runs `-check-config` through `sudo -u tanktemp`.

The unit (`tanktemp.service`) is hardened. For the water meter, it uses
`DevicePolicy=closed` with `DeviceAllow=char-gpiochip r` (read-only is
enough to request a line) and
`SupplementaryGroups=gpio` in place of `PrivateDevices=true`, which would
hide the GPIO chips. What else `PrivateDevices` implied is kept: the
`@raw-io` system calls are filtered, and `CapabilityBoundingSet` already
drops `CAP_SYS_RAWIO` and `CAP_MKNOD`. Two extras go beyond it: native
architecture only, and `/dev/shm` hidden. `systemd-analyze security` scores it 3.6, against 3.7
before (lower is better). The service user isn't added to `gpio`; only the
service gets it. Both scripts refuse a Pi with no `gpio` group (Raspberry
Pi OS makes it), because systemd can't start the unit without it.

**Deploy the build before the config.** A build from before the meter
refuses a config with a `meters` block, so on a Pi that's behind,
`./deploy.sh` first, then `./setup-pi.sh` to install the new config.

A Pi set up before this keeps its builds in `~/tanktemp`. To move it, run
`./setup-pi.sh --check`, then `./setup-pi.sh`: it copies the builds, repoints
the links and restarts once. It leaves `~/tanktemp` for you to remove.

**The 1-Wire bus pins are in the config** (`onewire.buses`, #10), next to
the meter's: the one place that says which pin is which. `setup-pi.sh` writes
`config.txt`'s `dtoverlay=w1-gpio,gpiopin=N` lines from them (`tanktemp
-config FILE -print-w1-gpios` prints them), shows the exact change with
`--check`, and only ever adds lines: one for a pin the config doesn't
declare is refused, named. It refuses a config without the block.
`-check-config` names the buses, or warns in capitals that they aren't
declared, and at start tanktemp checks the buses loaded are the ones
declared (`w1_bus_missing`, `w1_bus_undeclared`). The pins aren't a
`setup-pi.sh` option any more (`--w1-gpios` is gone).

The host can also go in `deploy/host` (gitignored). The config comes from
`deploy/config.local.yaml` (gitignored; start from
`deploy/config.example.yaml`). `./deploy_test.sh` tests the scripts against
stubs.

### sudo

The first `./setup-pi.sh` needs sudo on the Pi that asks for no password for
anything (full sudo): the scripts run over ssh and cannot type one. It
installs `/etc/sudoers.d/tanktemp` (root:root 0440, checked with
`visudo -c` before it goes in), which lets the deploy user run, with no
password, exactly the command lines the scripts use after that, each by its
full path: `systemctl restart tanktemp`, `systemctl daemon-reload`, three
`journalctl` reads of tanktemp's journal, the builds' `-check-config` as
`tanktemp`, and `cat`, `install` and `cmp` of `/etc/tanktemp/config.yaml` and
its staged copy in the deploy user's home. Nothing else, and no blanket
`NOPASSWD`. From then on, full sudo can go:

- `./deploy.sh` (and `--rollback`) needs no password.
- A config change needs none either: edit `deploy/config.local.yaml`, then
  `./setup-pi.sh --check` (shows the diff), then `./setup-pi.sh` (installs
  it, restarts tanktemp and checks `/healthz`).
  That's also how `log.verbose` goes on for testing and off again after
  ([Verbose logging](#verbose-logging-at-the-bench)).

Both scripts check before changing anything: full sudo, or else every one of
those rules. If neither, they refuse and name the command that wants a
password. With only the rules, `setup-pi.sh` refuses any step other than a
config change (a missing link, a new user, stopping templog), naming it,
before doing any of it: run it once more with full sudo for those. A run with
full sudo also puts the rules back as they should be if they have changed,
showing the diff first.

The rules keep deploys from needing root for anything else, and keep a
mistake in a script from doing more than these commands can. They do not stop
a determined deploy user: that user owns `/opt/tanktemp`, including the unit
file systemd runs (with `daemon-reload` and `restart` allowed) and the builds
that `-check-config` runs as `tanktemp`. A unit file they write can run
anything as root at the next allowed `daemon-reload` and `restart`.
