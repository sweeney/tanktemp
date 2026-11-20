# tanktemp: notes for Claude

Read `PLAN.md` (decisions), `docs/WIRE.md` (the contract) and
`docs/ONEWIRE.md` (the probes) or `docs/METER.md` (the water meter) before
changing behaviour.

## Rules

- **Strict red/green TDD.** Write the failing test, run it and see it fail,
  then make it pass. Report the red as well as the green.
- **The core is isolated.** `cadence`, `config`, `meter`, `payload`,
  `reading`, `startup`, `status` and `sweep` never import `os`, `io/fs`,
  `net`, `syscall`, the 1-Wire handler or MQTT. `boundaries_test.go`
  enforces it; add any new core package to its list. Outside effects go
  through interfaces: `reading.Source` for the probes, `meter.Source` for
  the meter's line, a `Publisher` declared where it's used, injected time
  (wall and monotonic).
- **`internal/onewire` is the only code that knows about sysfs and DS18B20
  quirks.** New hardware behaviour starts as a captured fixture
  (`cmd/w1capture`) with an `expect.json` that fails, then the fix. Real
  expected values come from the kernel's own `t=` line. Synthetic fixtures
  come from `testdata/gen_fixtures.py` and are marked `"synthetic": true`.
- **`internal/gpioedge` is the only code that knows about GPIO** (the
  character device, `go-gpiocdev`, kernel debounce and event numbers).
  `internal/meter`'s counter is pure. New line behaviour starts as a raw
  `gpiomon` capture under `internal/meter/testdata/` with an `expect.json`
  that fails, its values worked out independently of the counter (never by
  running it). The pin and every timing come from the config, never the
  code.
- **Hand-written fakes**, not mockgen: `readingtest.Fake`,
  `metertest.Fake`, `mqttpubtest.Fake`. `readingtest.Contract` must pass
  for every `reading.Source`, and `metertest.Contract` for every
  `meter.Source`.
- **The goldens in `internal/payload/testdata/` are the wire contract.**
  Never edit one to make a test pass. A change starts in `docs/WIRE.md`.
- **Unknown is `null`, never 0.** A failing probe is never left out of a
  sweep.
- **Guesses go in `GUESSES.md`.**
- **No PII:** no real hostnames, IPs, site names or house details in
  committed files, commits or PRs. The deploy host and the real config are
  gitignored (`deploy/host`, `deploy/config.local.yaml`).
- **Commits:** ask what date and time each should carry, and set both
  `GIT_AUTHOR_DATE` and `GIT_COMMITTER_DATE`. The build is stamped with the
  commit hash, so settle dates before deploying.
- **The Pi:** use `./deploy.sh` and `./setup-pi.sh`, `--check` first. Never
  edit `/boot/firmware/config.txt` and never reboot without asking.
- The common modules are pinned in `go.mod` to their tags. To try an
  unreleased change in common, use a gitignored `go.work` in this repo,
  never at `~/src/github.com/sweeney/`, and pin the new tag before pushing.
