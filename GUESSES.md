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
