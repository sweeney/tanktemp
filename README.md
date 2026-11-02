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
                                                               └──> internal/status ──> internal/web (/healthz, /api/status, /)
```

`cmd/tanktemp` wires these together with
[`common/lifecycle`](https://github.com/sweeney/common) (signals, STARTUP,
heartbeats, SHUTDOWN, cancellable sweeps), `common/mqttpub` (the resilient
publisher) and `common/health` (`/healthz`, `sysinfo`).

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
