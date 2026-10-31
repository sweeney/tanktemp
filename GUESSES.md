# Guesses made while building unattended

These are the choices I made without asking, while building overnight on
2026-10-31/11-01, so you can review each one. Each entry says what I
guessed, why, and what it would take to change it. Anything the plan already
decided isn't listed here.

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
