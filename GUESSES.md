# Guesses made while building unattended

These are the choices I made without asking, while building overnight on
2026-10-31/11-01, so you can review each one. Each entry says what I
guessed, why, and what it would take to change it. Anything the plan already
decided isn't listed here.

| # | Guess | Why | To change |
|---|---|---|---|
| G1 | CI (`.github/workflows/ci.yml`) is written but can't pass on GitHub until `lifecycle`, `health` and `mqttpub` are tagged in common. The local build uses a gitignored `go.work`. | Nothing is pushed yet, and the tags wait for tanktemp to run on the Pi. | Tag the modules, pin them in `go.mod`, then push. |
