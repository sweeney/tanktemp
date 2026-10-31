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

Until `lifecycle`, `health` and `mqttpub` are tagged in `sweeney/common`, the
build uses a gitignored `go.work` pointing at local checkouts of them.

## Deploying

```sh
./setup-pi.sh --check user@host   # read-only: what a first install would do
./setup-pi.sh user@host           # first install: config, unit, first build
./deploy.sh --check user@host     # read-only pre-flight
./deploy.sh user@host             # build, upload, switch, verify, roll back on failure
./deploy.sh --rollback user@host
```

The host can also go in `deploy/host` (gitignored). The config comes from
`deploy/config.local.yaml` (gitignored; start from
`deploy/config.example.yaml`). `./deploy_test.sh` tests the scripts against
stubs.
