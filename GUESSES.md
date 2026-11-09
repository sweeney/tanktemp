# Guesses made while building unattended

These are the choices I made without asking, while building overnight on
2026-10-31/11-01, so you can review each one. Each entry says what I
guessed, why, and what it would take to change it. Anything the plan already
decided isn't listed here.

## Your answers (2026-11-01)

You kept 19 of the 25 as they are. The other six:

| # | Your answer | Status |
|---|---|---|
| G5 | Talk me through capturing them, and keep the synthetic ones as well | done, 2026-11-01. Both faults are captured for real: `rig-power-on-reset-2026-11-01` (bytes `50 05 … 0c`, as the datasheet says) and `rig-bus-low-2026-11-01` (all `00`, `crc=00 YES`). The handler already classified every reading correctly. The synthetic fixtures stay as the minimal cases. `deploy/capture.sh` can capture more |
| G7 | Tighten the offset limit to ±2 °C | done: `config.MaxOffsetC` = 2 |
| G13 | Walk me through the wire contract, with a page of charts and flow diagrams | `docs/wire-walkthrough.html`. Building it found ten places where the docs and code disagreed, all now fixed. One was a real bug: `raw_c` was the kernel's truncated value (12.062 for 12.0625), not exact as WIRE.md promised. The goldens didn't change |
| G16 | A suspect 85 °C degrades health | done: `/healthz` is `degraded` while any probe is suspect. The value is still published |
| G24 | Remove templog after the cutover | after tanktemp has run for a week, from 2026-11-08 at the earliest (PLAN.md, build status) |
| G25 | A dedicated `tanktemp` system user | done in the scripts; the Pi isn't migrated yet. tanktemp runs as the `tanktemp` user from `/opt/tanktemp`, with `ProtectHome=yes` back on. The config is root:tanktemp 0640. No extra group is needed, because the 1-Wire files are world-readable. `setup-pi.sh` creates the user and moves an existing install over (README, "Deploying") |

Then the wire review (`docs/wire-walkthrough.html`, 2026-11-01): you kept
the prefix and topic shape and the retained/QoS choices, and confirmed G2,
G10, G14, G15 and G19. One change: `attempts` and `bus` are off the wire.
They're on tanktemp's status page and `/api/status` only (WIRE.md,
"Changes").

G1 is done: the modules are tagged and pinned, tanktemp is pushed, and CI runs on GitHub.

Since then: G20's log line names each fault, e.g. `sweep: 5 ok, 5 failed,
4.81s: bus_low: rig-bus-a/a1, rig-bus-a/a2, …`. A sweep whose only problem
is a suspect 85 °C gets a line too, because it now degrades health (G16).

## Verbose logging, issue #2 (2026-11-09)

The issue settled most of it. These are the choices it left open:

| # | Guess | Why | To change |
|---|---|---|---|
| G29 | **One verbose line per sweep, not per tank**, with every tank on it, separated by ` \| `: `sweep 4.0s 9/10 left 36.2 … \| right 39.9 …`. | The issue allowed either. One line keeps a sweep's tanks together, and `grep '^sweep '` still gets everything. | `sweep.Sweep.Line` |
| G30 | **Values to 0.1 °C, by device_id with no positions**, bottom (order 1) first, as the issue's example. A failed probe is `null`, with no code. | Short enough to read across a terminal. The fault line on the next line names the code and the position. | `sweep.Sweep.Line` |
| G31 | **Every failed attempt is logged, including the last**, with the handler's own error text. `not_present` is never logged as a retry. | The last attempt's error has detail the fault line's code lacks. A missing sensor isn't retried, and the fault line already names it. | `readChecked` in `internal/onewire/source.go` |
| G32 | **The 85 °C extra read is logged** with its outcome. | It's a retry in all but name (G2), and the one most worth seeing at the bench. | `recheck85` |
| G33 | **`-check-config` says `verbose log off` or `verbose log ON`, before `publishing to`.** | `deploy/lib.sh`'s `parse_check_config` reads the prefix and broker from the end of that line. The capitals make it stand out in `./deploy.sh --check`. | `realMain` |
| G34 | **No cadence lines yet.** The verbose logger is ready for them, but adaptive cadence (#1) isn't built. | Nothing to log until #1 lands. | #1 |
| G35 | **`verbose` is in `/api/status` only**, not `/healthz` or the MQTT status. | The issue asked for `/api/status`. The MQTT payloads are the wire contract, and this is a local diagnostic. | `web.apiStatus` |

The table below is the original record of each guess.

| # | Guess | Why | To change |
|---|---|---|---|
| G1 | CI (`.github/workflows/ci.yml`) is written but can't pass on GitHub until `lifecycle`, `health` and `mqttpub` are tagged in common. The local build uses a gitignored `go.work`. | Nothing is pushed yet, and the tags wait for tanktemp to run on the Pi. | Tag the modules, pin them in `go.mod`, then push. |
| G2 | **The 85 °C extra read sits outside `attempts`.** An accepted 85.000 always gets one more read, even on the last attempt, so a reading can report 4 attempts when `attempts` is 3. If that read gives a different good value, the new value replaces the 85; otherwise the 85 is kept with `suspect: 85c_unverified`. | The plan said "retry once more". It didn't say whether the extra read counts as one of the 3. Leaving it outside means an 85 is never published unchecked. | `Source.recheck85` in `internal/onewire/source.go` |
| G3 | **Unrecognised read errors** (anything that isn't a parse or scratchpad failure, or a missing file) map to `read_error` and **are retried**. | Every read is a fresh conversion, so an unknown transient error probably clears like the known ones. | `readChecked` |
| G4 | **`ext_power` other than 0 or 1, or an unparseable `resolution`, is reported as unknown (`null`)**, not as an error. | Unknown values are absent, never guessed. | `Source.Inspect` |
| G5 | **The synthetic fixtures** (`internal/onewire/testdata/sysfs/synthetic-*`) model faults from the byte patterns in ONEWIRE.md. Two of them were never seen with full scratchpads: the power-on reset and the bus held low. | They need your hands on the hardware to capture for real. Each is marked `"synthetic": true`. | Capture with `w1capture`, then replace the directory |
| G6 | **`sweep.interval` must be at least 0.8 s × the total number of configured sensors**: one clean read of every sensor, with all of them on one bus. | Config can't know which bus each ROM is on, so it assumes the worst case. A sweep that fails and retries can still run past the interval; the lifecycle then skips a tick rather than queueing it. | `config.ReadTime` |
| G7 | **`offset_c` beyond ±5 °C is refused** as a probable typo. | DS18B20s are within ±0.5 °C out of the box; 5 °C leaves room for a surface mount that reads low. | `config.MaxOffsetC` |
| G8 | **The plan's `sweep.parallel` option is dropped.** Buses are always read in parallel. | No reason was found to read them in series, and fewer knobs means fewer combinations to test. | Re-add it to `config.Sweep` and `onewire.Source` |
| G9 | **An empty `mqtt.client_id` becomes `tanktemp-<hostname>`**, filled in by `main`. | `config` doesn't touch the OS. | `cmd/tanktemp` |
| G10 | **Calibrated values are rounded to 4 decimal places**, which removes float noise (61.375 − 0.19 publishes as 61.185). `raw_c` is exact, a multiple of 1/16 °C. | Readings come in 1/16 °C steps and offsets in hundredths of a degree, so 4 places never lose information. | `sweep.calibrate` |
| G11 | **When discovery fails** (e.g. no bus masters), the sweep still runs, every sensor reads `not_present`, and the error is recorded in the sweep (and so in health). | Fail loud, and every configured sensor appears in every sweep. | `sweep.Sweeper.Sweep` |
| G12 | **The startup checks only flag what they actually see:** a sensor on no bus, parasite power, a resolution other than 12, an `Inspect` that fails, a discovery that fails. An unreadable `ext_power` or `resolution` file isn't flagged. | No speculative checks. | `internal/startup` |
| G13 | **The wire contract (`docs/WIRE.md` and its goldens) was written without your review.** The plan said I would show it to you before writing the encoders. | You were asleep and said to proceed. Nothing subscribes to these topics yet, and the test deploy uses the `test/` prefix, so a change now costs nothing. | Edit `docs/WIRE.md` and the goldens together, then `internal/payload` |
| G14 | **SHUTDOWN publishes `online: false`. The last will is exactly `{"online":false,"event":"LWT"}`, with no timestamp**, unlike mqttpub's `OfflinePayload`, which adds one. | A will is fixed when tanktemp connects, so a timestamp in it would be the connect time and misleading. | `payload.LWT` |
| G15 | **Status messages carry `startup_problems[].message`, a human-readable sentence**, alongside the stable `code`. | It's the most useful thing on a dashboard. Consumers should match on `code`, never the message. | `payload.Status` |
| G16 | **A `suspect` (85 °C) reading doesn't degrade health.** It is published, with a flag. | It's probably a real value, and degrading health would make a hot cylinder look like a fault. | `status.Snapshot.Health` |
| G17 | **"Stale" means the last sweep is more than 3 × the interval old.** | It's what the plan said. With a 30 s interval, health degrades after 90 s with no sweep. | `status.StaleAfter` |
| G18 | **Startup waits at most 10 s for the broker**, then sweeps and serves HTTP anyway while the connection retries in the background. A dead broker never stops the sensors being read. | STARTUP normally gets through, and a broker outage at boot doesn't take the service down with it. | `main.ConnectWait` |
| G19 | **After every MQTT reconnect, the status is republished as a HEARTBEAT**, replacing the last will the broker may have published while tanktemp was away. | Otherwise the retained status would read `online:false` until the next scheduled heartbeat, up to 15 minutes later. | `run`, the `Reconnected` goroutine |
| G20 | **A sweep with failures logs one line per sweep**, which means every 30 s for a sensor that stays missing. | Simple, and journald copes. If it's too noisy, log only when the set of failures changes. | `run`'s Work |
| G21 | **`mqttpubtest.Fake` now takes a lock and gained `Snapshot()`** (mqttpub commit 6134c56). That's a small API addition to the module the extraction agent built. | tanktemp publishes from two goroutines while its test polls. Fixed at the source rather than wrapped in tanktemp. | `mqttpub/mqttpubtest/fake.go` |
| G22 | **Deploy health:** `deploy.sh` accepts a new build when `/healthz` reports its version, `ready`, `mqtt_connected`, a status other than `starting`, and no fewer `sensors_ok` than before. A `degraded` status is only a warning. | One probe already missing shouldn't block a deploy, while a build that breaks reading still fails it. | `wait_healthy` in `deploy/lib.sh` |
| G23 | **`--allow-unpushed`** lets `deploy.sh` and `setup-pi.sh` run from a local repo. The default still refuses an unpushed commit, and `setup-pi.sh` has no `--allow-dirty`. | Overnight deploys from an unpushed repo. Note that the build on the Pi is stamped with a local commit hash, and **rewriting commit dates later changes the hashes**. Redeploy after settling them. | |
| G24 | **`setup-pi.sh` treats `templog` as a systemd unit of that name.** It checks for no timer or cron entry, and it restarts tanktemp after a config change (after a prompt). | That's how the rig runs it. | `setup-pi.sh` |
| G25 | **tanktemp runs as `User=sweeney`, not a `DynamicUser`.** The first install showed why: the builds live in `~sweeney/tanktemp`, the home directory is mode 0700, and a throwaway user can't reach it (`status=203/EXEC`). All the other hardening stays, with `ProtectHome=read-only` as in boiler-sensor. | It was the smallest fix that keeps the deploy layout boiler-sensor uses. | Move the builds to `/opt/tanktemp` and go back to `DynamicUser=yes`. That's a change to the deploy scripts |
| G26 | **Scoped sudo is detected with `sudo -n -l CMD` for each rule, plus one real run** of `sudo -n journalctl -u tanktemp -n 30 --no-pager -o cat`, a complaint from sudo itself (`sudo: …`) meaning the rules aren't in force. | `sudo -l` says a command is allowed even when the rule that allows it wants a password (say `%sudo ALL=(ALL) ALL`, if some other rule has `NOPASSWD`), so `-l` alone could pass and the deploy then stop at a password. Not yet tried against the real sudo on the Pi: run `./deploy.sh --check` there first. | `sudo_probe` in `deploy/lib.sh` |
| G27 | **The sudoers rules name commands by the path `command -v` finds on Debian's `secure_path`**, escape `=` as `\=` (sudoers needs it in arguments), use `*` only for `--since`, the invocation ID and the build name, and go in through `/etc/sudoers.d/.tanktemp.incoming` (sudo ignores names with a dot), checked there with `visudo -c -f`, then `mv`'d into place. | It's the sudo behaviour I know of, but the fake sudo in `deploy_test.sh` only approximates it (its `*` also matches `/` in the command path, where sudo's doesn't). | `sudo_rules`, `sudoers_fragment` in `deploy/lib.sh`; the sudoers step in `setup-pi.sh` |
| G28 | **With only the scoped rules, `setup-pi.sh` does a config change and the restart onto it, and nothing else**, even a step a rule would allow (a lone `daemon-reload`, starting a stopped service). A config the Pi has none of yet needs full sudo, as it may need `install -d`. | It's what was asked for, and keeps the scoped path small. | The `SUDO_MODE = scoped` check in `setup-pi.sh` |
