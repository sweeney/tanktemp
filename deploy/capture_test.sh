#!/usr/bin/env bash
# Tests for deploy/capture.sh. ssh runs the "remote" command locally against a
# fake /sys and /tmp; scp copies; go "builds" a fake w1capture that copies
# fixture trees into its step directories. No network, no Pi, no Go build.
#
# Usage: ./deploy/capture_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SYSFS="$HERE/../internal/onewire/testdata/sysfs"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/capture-test.XXXXXX") # honours TMPDIR on macOS too
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"
mkdir -p "$STUBS"

# ssh HOST CMD: run CMD here, with the Pi's /sys/bus/w1/devices and /tmp
# mapped into the fake Pi. FAKE_SSH_FAIL makes every call fail.
cat >"$STUBS/ssh" <<'EOF'
#!/usr/bin/env bash
[ -z "${FAKE_SSH_FAIL:-}" ] || exit 255
shift # host
cmd="$*"
# Only the capture's own /tmp paths: the fake Pi may itself be under /tmp.
cmd=${cmd//\/tmp\/tanktemp-capture/$FAKE_PI/tmp/tanktemp-capture}
cmd=${cmd//\/sys\/bus\/w1\/devices/$FAKE_PI/sys}
echo "ssh: $*" >>"$FAKE_LOG"
bash -c "$cmd"
EOF
cat >"$STUBS/scp" <<'EOF'
#!/usr/bin/env bash
[ "$1" = -q ] && shift
dst=${2#*:}
dst=${dst//\/tmp\/tanktemp-capture/$FAKE_PI/tmp/tanktemp-capture}
echo "scp: $*" >>"$FAKE_LOG"
cp "$1" "$dst"
EOF
# go build -o OUT ./cmd/w1capture: a fake w1capture. Step 1 copies
# FAKE_FIRST (a calm bus); every later step copies FAKE_THEN (the fault).
cat >"$STUBS/go" <<'EOF'
#!/usr/bin/env bash
echo "go: $* GOOS=$GOOS GOARCH=$GOARCH" >>"$FAKE_LOG"
while [ $# -gt 0 ]; do [ "$1" = -o ] && out="$2"; shift; done
cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "w1capture: $*" >>"$FAKE_LOG"
while [ $# -gt 0 ]; do
    case "$1" in -out) out="$2" ;; -steps) steps="$2" ;; esac; shift
done
for i in $(seq 1 "$steps"); do
    src="$FAKE_THEN"; [ "$i" -eq 1 ] && src="$FAKE_FIRST"
    d="$out/$(printf 'step-%03d' "$i")"; mkdir -p "$d"
    (cd "$src" && tar --exclude expect.json -cf - .) | tar -C "$d" -xf -
    echo "captured $d"
done
BIN
chmod +x "$out"
EOF
chmod +x "$STUBS/"*

fresh() {
    export FAKE_PI="$WORK/pi-$RANDOM" FAKE_LOG="$WORK/log-$RANDOM" CAPTURE_FIXTURES="$WORK/fixtures-$RANDOM"
    mkdir -p "$FAKE_PI/sys" "$FAKE_PI/tmp" "$CAPTURE_FIXTURES"
    : >"$FAKE_LOG"
    # The fake Pi's /sys: a copy of the calm rig.
    cp -R "$SYSFS/rig-steady/." "$FAKE_PI/sys/"
    rm -f "$FAKE_PI/sys/expect.json"
    export FAKE_FIRST="$SYSFS/rig-steady" FAKE_THEN="$SYSFS/rig-steady"
    unset FAKE_SSH_FAIL || true
}
run() { set +e; OUT=$(PATH="$STUBS:$PATH" "$HERE/capture.sh" "$@" 2>&1 </dev/null); RC=$?; set -e; }
dest() { echo "$CAPTURE_FIXTURES/rig-$1-$(date +%Y-%m-%d)"; }

pass=0 fail=0
check() {
    local name="$1"; shift
    if "$@"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $name"; while IFS= read -r l; do echo "  | $l"; done <<<"$OUT"; fi
}
says() { grep -qE -- "$1" <<<"$OUT"; }
rc_is() { [ "$RC" -eq "$1" ]; }
logged() { grep -qE -- "$1" "$FAKE_LOG"; }
not() { ! "$@"; }
remote_clean() { [ -z "$(ls -A "$FAKE_PI/tmp")" ]; }

# --check: preflight only.
fresh
run --check --host pi@example power-on-reset
check "check: ok" rc_is 0
check "check: counts the bus" says "ok: [0-9]+ buses, [0-9]+ DS18B20s"
check "check: says the plan" says "Would capture 15 snapshots, 0s apart"
check "check: says what you'd do" says "VDD"
check "check: builds nothing" not logged "^go:"
check "check: copies nothing" not logged "^scp:"
check "check: creates no fixture" test ! -e "$(dest power-on-reset)"

# A power-on reset, caught.
fresh
export FAKE_THEN="$SYSFS/synthetic-power-on-reset"
run --yes --host pi@example power-on-reset
check "por: ok" rc_is 0
check "por: builds for the Pi" logged "^go: build -o .*/w1capture ./cmd/w1capture GOOS=linux GOARCH=arm64"
check "por: the plan reaches w1capture" logged "^w1capture: -out .*/tmp/tanktemp-capture-power-on-reset -steps 15 -interval 0s"
check "por: copied back" test -f "$(dest power-on-reset)/step-015/28-0000117287e6/w1_slave"
check "por: names the reading" says "caught: step-002 28-0000117287e6: 50 05 .* 0c 10"
check "por: not step 1 (calm)" not says "caught: step-001"
check "por: counts" says "caught in 14 reading"
check "por: remote cleaned up" remote_clean
check "por: points at expect.json" says "expect.json"

# Bus held low, caught; with overrides.
fresh
export FAKE_THEN="$SYSFS/synthetic-bus-low"
run --yes --host pi@example --steps 3 --interval 2s bus-low
check "low: ok" rc_is 0
check "low: overrides reach w1capture" logged "^w1capture: .* -steps 3 -interval 2s"
check "low: caught all zeros" says "caught: step-002 .*: 00 00 00 00 00 00 00 00 00"

# Nothing caught: say so, keep the run.
fresh
run --yes --host pi@example bus-low
check "missed: ok" rc_is 0
check "missed: says so" says "wasn't caught"
check "missed: run kept" test -d "$(dest bus-low)/step-008"

# Without --yes it waits for Enter.
fresh
set +e; OUT=$(printf '\n' | PATH="$STUBS:$PATH" "$HERE/capture.sh" --host pi@example --steps 2 bus-low 2>&1); RC=$?; set -e
check "prompt: proceeds on Enter" rc_is 0
check "prompt: asked" says "Press Enter"
fresh
set +e; OUT=$(PATH="$STUBS:$PATH" "$HERE/capture.sh" --host pi@example bus-low 2>&1 </dev/null); RC=$?; set -e
check "prompt: no answer, no capture" not logged "^w1capture:"
check "prompt: no answer fails" not rc_is 0

# Refusals.
fresh
mkdir -p "$(dest bus-low)"
run --yes --host pi@example bus-low
check "exists: refused" rc_is 1
check "exists: says so" says "already exists"
check "exists: nothing built" not logged "^go:"

fresh
rm -rf "$FAKE_PI/sys/"w1_bus_master*
run --check --host pi@example bus-low
check "no buses: refused" rc_is 1
check "no buses: says so" says "no 1-Wire bus masters"

fresh
export FAKE_SSH_FAIL=1
run --check --host pi@example bus-low
check "no ssh: refused" rc_is 1
check "no ssh: says so" says "can't ssh"

fresh
run --host pi@example sideways
check "unknown scenario" rc_is 1
run --host pi@example
check "no scenario" rc_is 2
run --host pi@example --steps 1 bus-low
check "steps < 2" rc_is 1
run --host pi@example --interval soon bus-low
check "bad interval" rc_is 1
run --host pi@example bus-low power-on-reset
check "two scenarios" rc_is 1
run --help
check "help" says "Usage: deploy/capture.sh"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
