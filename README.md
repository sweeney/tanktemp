# tanktemp

Reads DS18B20 1-Wire temperature probes fitted to unvented hot water
cylinders, on a Raspberry Pi. It publishes every sweep to MQTT and serves
`/healthz` and a status page. Any number of cylinders, and any number of
probes on each, is set in a local config file.

- **Wire contract:** [`docs/WIRE.md`](docs/WIRE.md), pinned by the goldens
  in `internal/payload/testdata/`.
- **For statehouse:** [`docs/STATEHOUSE.md`](docs/STATEHOUSE.md), the
  adapter spec.
- **The hardware, and everything learned on the test rig:**
  [`docs/ONEWIRE.md`](docs/ONEWIRE.md).
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
```

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
- **`/api/status` shows it**, for tuning: `sweep.mode` (`slow`, `hold`,
  `decay`), `interval_s`, `next_due`, `last_trigger` (probe and rate) and
  `fast_entries`. The status page counts down to the next sweep. With
  `log.verbose` on, every trigger and change of mode is logged (below).
  `-check-config` says what the cadence is.

Every key in `fast` has the default shown, so `fast: {}` turns it on. The
trigger and hold are guesses from two days of recordings (GUESSES G38,
G39); tune them from `/api/status`.

## Testing

```sh
go test -race ./...
```

No hardware, root or broker needed. There are two layers:

- **Core logic** (`config`, `sweep`, `payload`, `startup`, `status`) sees the
  sensors only through `reading.Source`, and is tested against the
  scriptable `readingtest.Fake`. `boundaries_test.go` fails the build if a
  core package imports `os`, `io/fs`, `net`, `syscall`, the 1-Wire handler or
  MQTT.
- **The 1-Wire handler** (`internal/onewire`) is tested against real sysfs
  trees captured from the rig with `cmd/w1capture`, plus synthetic faults
  generated from them (`internal/onewire/testdata/gen_fixtures.py`). Every
  directory under `testdata/sysfs` is a test case with its own
  `expect.json`. There are fuzz targets, every raw register value is
  checked, and the reading contract suite runs against both the fake and the
  real handler.

To capture a new hardware state as a fixture (read-only on the Pi):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o w1capture ./cmd/w1capture
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
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -tags hardware -o onewire.test ./internal/onewire/
scp onewire.test user@host:/tmp/
ssh user@host 'TANKTEMP_ROMS=28-…,28-… /tmp/onewire.test -test.run Hardware -test.v; rm /tmp/onewire.test'
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
  cadence: decay 10.4s
  cadence: slow 30s
  ```

## Reviewing the guesses

`go run ./tools/guessreview -open` serves a local page (loopback only, port
8741) for answering `GUESSES.md` one guess at a time. Each answer is saved
as it's given to `tools/guessreview/answers.json`, which is gitignored.

## Deploying

```sh
./setup-pi.sh --check user@host   # read-only: what a first install would do
./setup-pi.sh user@host           # first install (full sudo): sudo rules, config, unit, first build
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

A Pi set up before this keeps its builds in `~/tanktemp`. To move it, run
`./setup-pi.sh --check`, then `./setup-pi.sh`: it copies the builds, repoints
the links and restarts once. It leaves `~/tanktemp` for you to remove.

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
