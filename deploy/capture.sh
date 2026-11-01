#!/usr/bin/env bash
# Guided capture of a 1-Wire fault on the Pi, as a real test fixture.
#
# It builds cmd/w1capture for the Pi, copies it over, tells you what to do
# with your hands, captures a run of snapshots while you do it, copies the
# run back to internal/onewire/testdata/sysfs/rig-<scenario>-<date>/, and
# says whether the fault was caught. Read-only on the Pi: no sudo, nothing
# stopped. tanktemp keeps running, so it publishes the fault too (watch it
# with ./tools/live.sh).
#
# Usage: deploy/capture.sh [--check] [--yes] [--host user@host] SCENARIO
#   SCENARIO    power-on-reset | bus-low
#   --check     check the preconditions only; capture nothing, copy nothing
#   --yes       skip the "ready?" prompt (for scripted use)
#   --host      ssh target (default: the contents of deploy/host)
#   --steps N / --interval D   override the scenario's capture plan

# Remote paths and arguments are expanded here on purpose, then sent to the Pi.
# shellcheck disable=SC2029
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
FIXTURES="${CAPTURE_FIXTURES:-$ROOT/internal/onewire/testdata/sysfs}" # overridable for tests
CHECK=0 YES=0 HOST="" SCENARIO="" STEPS="" INTERVAL=""

die() { echo "capture.sh: $*" >&2; exit 1; }
usage() { sed -n '2,/^$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --yes) YES=1 ;;
        --host) [ $# -ge 2 ] || die "--host needs user@host"; HOST="$2"; shift ;;
        --steps) [ $# -ge 2 ] || die "--steps needs a number"; STEPS="$2"; shift ;;
        --interval) [ $# -ge 2 ] || die "--interval needs a duration"; INTERVAL="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        -*) die "unknown option $1 (try --help)" ;;
        *) [ -z "$SCENARIO" ] || die "one scenario at a time"; SCENARIO="$1" ;;
    esac
    shift
done

case "$SCENARIO" in
    power-on-reset)
        PLAN_STEPS=15 PLAN_INTERVAL=0s
        DOING="pull ONE probe's power (VDD, usually red) wire for about a second, then push it back"
        HOW="Every 10 s or so, while the capture runs, $DOING. Do it five or six times.
   Its first read after power returns should be the power-on value: 85 °C
   with byte 6 = 0c. Don't touch the data or ground wires, and never bridge
   two pins."
        ;;
    bus-low)
        PLAN_STEPS=8 PLAN_INTERVAL=5s
        DOING="unplug ONE bus's data jumper (Pi GPIO to the board), or its pull-up if that isn't soldered"
        HOW="After the 2nd 'captured' line, $DOING.
   Keep the loose end clear of other pins. Plug it back after the 5th.
   This reproduces the data-line fault of 2026-09-28 (the pull-up contact
   failed): all bytes 00 (t=0), then the sensors drop off the bus. The line
   may float high instead (all ff, crc NO); that's worth having too. Don't
   short the data line to ground to fake it."
        ;;
    "") usage >&2; exit 2 ;;
    *) die "unknown scenario '$SCENARIO' (power-on-reset or bus-low)" ;;
esac
STEPS=${STEPS:-$PLAN_STEPS}
INTERVAL=${INTERVAL:-$PLAN_INTERVAL}
if ! [[ "$STEPS" =~ ^[0-9]+$ ]] || [ "$STEPS" -lt 2 ]; then
    die "--steps must be a number, at least 2"
fi
[[ "$INTERVAL" =~ ^[0-9]+(ms|s|m)$ ]] || die "--interval must look like 0s, 500ms, 5s or 1m"

if [ -z "$HOST" ]; then
    [ -f "$ROOT/deploy/host" ] || die "no --host and no deploy/host file"
    HOST=$(tr -d '[:space:]' <"$ROOT/deploy/host")
fi
[ -n "$HOST" ] || die "empty host"

STAMP=$(date +%Y-%m-%d)
DEST="$FIXTURES/rig-$SCENARIO-$STAMP"
REMOTE="/tmp/tanktemp-capture-$SCENARIO"

# ---------------------------------------------------------------- preflight
echo "Checking the Pi ($HOST)…"
buses=$(ssh "$HOST" 'ls -d /sys/bus/w1/devices/w1_bus_master* 2>/dev/null | wc -l') || die "can't ssh to $HOST"
buses=$(tr -d '[:space:]' <<<"$buses")
[ "$buses" -gt 0 ] || die "$HOST has no 1-Wire bus masters"
sensors=$(ssh "$HOST" 'ls -d /sys/bus/w1/devices/28-* 2>/dev/null | wc -l' | tr -d '[:space:]')
echo "ok: $buses buses, $sensors DS18B20s on $HOST"
[ ! -e "$DEST" ] || die "$DEST already exists; move it aside or capture another day"
command -v go >/dev/null || die "go isn't installed here"

if [ "$CHECK" -eq 1 ]; then
    echo "Would capture $STEPS snapshots, $INTERVAL apart, into $DEST"
    echo "You would: $DOING"
    echo "--check: nothing captured"
    exit 0
fi

# ---------------------------------------------------------------- build + copy
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; ssh "$HOST" "rm -rf $REMOTE $REMOTE.w1capture" >/dev/null 2>&1 || true' EXIT
echo "Building w1capture for the Pi…"
(cd "$ROOT" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$WORK/w1capture" ./cmd/w1capture)
scp -q "$WORK/w1capture" "$HOST:$REMOTE.w1capture"
ssh "$HOST" "rm -rf $REMOTE"

# ---------------------------------------------------------------- capture
cat <<EOF

== $SCENARIO: $STEPS snapshots, $INTERVAL apart (each reads every sensor, ~0.8 s each)

   $HOW

EOF
if [ "$YES" -eq 0 ]; then
    printf 'Press Enter when your hands are ready (capture starts at once), or Ctrl-C to stop: ' >&2
    read -r _ || die "no answer; nothing captured"
fi
ssh "$HOST" "$REMOTE.w1capture -out $REMOTE -steps $STEPS -interval $INTERVAL"

# ---------------------------------------------------------------- copy back
mkdir -p "$FIXTURES"
ssh "$HOST" "tar -C $REMOTE -cf - ." | (mkdir "$DEST" && tar -C "$DEST" -xf -)
echo
echo "Copied to ${DEST#"$ROOT"/}"

# ---------------------------------------------------------------- what was caught
# A w1_slave's first line holds the nine scratchpad bytes. Power-on reset:
# raw 0x0550 (bytes 50 05) with byte 6 = 0c. Bus low: all nine bytes 00.
found=0
while IFS= read -r f; do
    bytes=$(head -1 "$f" | cut -c1-26)
    step=$(basename "$(dirname "$(dirname "$f")")")
    rom=$(basename "$(dirname "$f")")
    case "$SCENARIO" in
        power-on-reset) [[ "$bytes" =~ ^50\ 05\ [0-9a-f]{2}\ [0-9a-f]{2}\ [0-9a-f]{2}\ [0-9a-f]{2}\ 0c ]] || continue ;;
        bus-low) [ "$bytes" = "00 00 00 00 00 00 00 00 00" ] || continue ;;
    esac
    echo "caught: $step $rom: $(head -1 "$f")"
    found=$((found + 1))
done < <(find "$DEST" -name w1_slave | sort)
missing=$(find "$DEST" -mindepth 1 -maxdepth 1 -name 'step-*' | while read -r s; do
    for b in "$s"/w1_bus_master*/w1_master_slaves; do [ -f "$b" ] && cat "$b"; done | grep -c '^28-' || true; done | sort -u | tr '\n' ' ')

if [ "$found" -gt 0 ]; then
    echo "The fault was caught in $found reading(s)."
else
    echo "The fault wasn't caught in any reading. (Sensors per step: $missing)"
    echo "The run is kept anyway; try again with more steps, e.g. --steps 30."
fi
cat <<EOF

Next: the fixture needs an expect.json. Per CLAUDE.md, write it from what the
bytes show so TestScenarios fails, then fix the handler if it disagrees. The
synthetic-$SCENARIO fixture stays alongside it.
EOF
