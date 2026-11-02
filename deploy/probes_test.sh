#!/usr/bin/env bash
# Tests for deploy/probes.sh. ssh runs the "remote" command locally against
# a fake /sys/bus/w1/devices; for --watch, each ssh call can see the next of
# a series of fake trees. No network, no Pi.
#
# Usage: ./deploy/probes_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SYSFS="$HERE/../internal/onewire/testdata/sysfs"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/probes-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"
mkdir -p "$STUBS"

# ssh HOST CMD. FAKE_TREES is a space-separated list of trees: the Nth
# presence poll (a command that reads w1_master_slaves) sees the Nth tree
# (the last one repeats); other commands see the current tree.
cat >"$STUBS/ssh" <<'EOF'
#!/usr/bin/env bash
[ -z "${FAKE_SSH_FAIL:-}" ] || exit 255
shift
cmd="$*"
read -r -a trees <<<"$FAKE_TREES"
n=$(cat "$FAKE_STATE/polls" 2>/dev/null || echo 0)
if [[ "$cmd" == *w1_master_slaves* ]]; then n=$((n + 1)); echo "$n" >"$FAKE_STATE/polls"; fi
i=$(( n < 1 ? 0 : n - 1 )); [ "$i" -lt "${#trees[@]}" ] || i=$(( ${#trees[@]} - 1 ))
echo "ssh: $cmd" >>"$FAKE_STATE/log"
bash -c "${cmd//\/sys\/bus\/w1\/devices/${trees[$i]}}"
EOF
chmod +x "$STUBS/ssh"

# Trees: the rig as captured; the rig with nothing on bus 1; bus 1 with
# just one probe back; that probe failing its CRC.
tree() { local d="$WORK/trees/$1"; mkdir -p "$d"; cp -R "$SYSFS/rig-steady/." "$d/"; rm -f "$d/expect.json"; echo "$d"; }
FULL=$(tree full)
EMPTY=$(tree empty); printf 'not found.\n' >"$EMPTY/w1_bus_master1/w1_master_slaves"
ONE=$(tree one); printf '28-00001172de60\n' >"$ONE/w1_bus_master1/w1_master_slaves"
BAD=$(tree bad); cp "$ONE/w1_bus_master1/w1_master_slaves" "$BAD/w1_bus_master1/"
printf 'ff ff ff ff ff ff ff ff ff : crc=c9 NO\nff ff ff ff ff ff ff ff ff t=-62\n' >"$BAD/28-00001172de60/w1_slave"

cat >"$WORK/config.yaml" <<'EOF'
mqtt: {broker: tcp://broker.example:1883}
tanks:
  - device_id: hot-water-cylinder
    sensors:
      - {rom: 28-00001172de60, position: top, order: 2, height_mm: 1500}
      - rom: 28-00001171f570
        position: bottom
        order: 1
EOF

fresh() {
    export FAKE_STATE="$WORK/state-$RANDOM" FAKE_TREES="$1"
    mkdir -p "$FAKE_STATE"; : >"$FAKE_STATE/log"
    unset FAKE_SSH_FAIL || true
}
run() { set +e; OUT=$(PATH="$STUBS:$PATH" "$HERE/probes.sh" --host pi@example --config "$WORK/config.yaml" "$@" 2>&1 </dev/null); RC=$?; set -e; }

pass=0 fail=0
check() {
    local name="$1"; shift
    if "$@"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $name"; while IFS= read -r l; do echo "  | $l"; done <<<"$OUT"; fi
}
says() { grep -qE -- "$1" <<<"$OUT"; }
rc_is() { [ "$RC" -eq "$1" ]; }
not() { ! "$@"; }
lines_matching() { grep -cE -- "$1" <<<"$OUT" || true; }
no_writes() { ! grep -qE '(^|[;&| ])(rm|mv|cp|tee|echo [^|]*>|sudo|systemctl|chmod|chown) ' "$FAKE_STATE/log"; }

# The list.
fresh "$FULL"
run
check "list: ok" rc_is 0
check "list: all ten" [ "$(lines_matching '^w1_bus_master[12] +28-')" -eq 10 ]
check "list: a configured probe, flow style" says '^w1_bus_master1 +28-00001172de60 +[0-9]+\.[0-9]{3} °C +hot-water-cylinder/top$'
check "list: a configured probe, block style" says '^w1_bus_master1 +28-00001171f570 +[0-9]+\.[0-9]{3} °C +hot-water-cylinder/bottom$'
check "list: the kernel's value" says '28-00001172de60 +16\.250 °C'
check "list: an unconfigured probe" says '28-000011721aa5 +[0-9.]+ °C +not in config$'
check "list: counts" says '^10 probes on 2 buses, 2 in config$'
check "list: changes nothing on the Pi" no_writes

# A failing read says why rather than showing a number.
fresh "$BAD"
run
check "bad read: ok" rc_is 0
check "bad read: crc" says '28-00001172de60 +crc +hot-water-cylinder/top'
check "an empty bus is listed as empty" says '^w1_bus_master2 '

fresh "$EMPTY"
run
check "empty bus: says so" says '^w1_bus_master1 +\(no probes\)'

# Watch: a probe arriving and leaving.
fresh "$EMPTY $ONE $ONE $EMPTY"
run --watch --interval 0 --count 4
check "watch: ok" rc_is 0
check "watch: starts with what's there" says 'watching 2 buses: 0 on w1_bus_master1, 5 on w1_bus_master2'
check "watch: arrival, with bus, temp and config" says '\+ 28-00001172de60 on w1_bus_master1: 16\.250 °C \(hot-water-cylinder/top\)'
check "watch: arrival reported once" [ "$(lines_matching '\+ 28-00001172de60')" -eq 1 ]
check "watch: departure" says '\- 28-00001172de60 left w1_bus_master1'
check "watch: presence polls don't read temperatures" [ "$(grep -c 'w1_slave' "$FAKE_STATE/log")" -eq 1 ]
check "watch: changes nothing on the Pi" no_writes

# An unconfigured arrival.
fresh "$EMPTY $(tree spare)"
printf '28-000011721aa5\n' >"$WORK/trees/spare/w1_bus_master1/w1_master_slaves"
run --watch --interval 0 --count 2
check "watch: unconfigured arrival" says '\+ 28-000011721aa5 on w1_bus_master1: [0-9.]+ °C \(not in config\)'

# Two arriving together: hold one, and watch names the one that warms.
TWO=$(tree two); printf '28-00001172de60\n28-00001171f570\n' >"$TWO/w1_bus_master1/w1_master_slaves"
WARM=$(tree warm); cp "$TWO/w1_bus_master1/w1_master_slaves" "$WARM/w1_bus_master1/"
f="$WARM/28-00001171f570/w1_slave"; t=$(grep -o 't=-*[0-9]*' "$f" | cut -d= -f2)
sed -i.bak "s/t=$t/t=$((t + 1000))/" "$f" && rm -f "$f.bak"
fresh "$EMPTY $TWO $TWO $WARM"
run --watch --interval 0 --count 4
check "two: both reported" [ "$(lines_matching '^[0-9:]+ \+ 28-')" -eq 2 ]
check "two: asks for a hand" says '2 arrived together: hold ONE in your hand'
check "two: names the warm one" says '28-00001171f570 is warming \(\+1\.0 °C\): it.s the one in your hand \(hot-water-cylinder/bottom\)'
check "two: not the other" not says '28-00001172de60 is warming'

# --local: run on the Pi itself, no ssh. PROBES_SHELL stands in for bash
# so the tests can show it the fake trees, as the ssh stub does.
cat >"$STUBS/localsh" <<'EOF2'
#!/usr/bin/env bash
[ "$1" = -c ] || exit 99
echo "local: $2" >>"$FAKE_STATE/local"
exec "$(dirname "$0")/ssh" pi "$2"
EOF2
chmod +x "$STUBS/localsh"
runlocal() { set +e; OUT=$(PATH="$STUBS:$PATH" PROBES_SHELL="$STUBS/localsh" "$HERE/probes.sh" --local "$@" 2>&1 </dev/null); RC=$?; set -e; }

fresh "$FULL"
runlocal --config "$WORK/config.yaml"
check "local: ok" rc_is 0
check "local: all ten" [ "$(lines_matching '^w1_bus_master[12] +28-')" -eq 10 ]
check "local: the kernel's value and config" says '28-00001172de60 +16\.250 °C +hot-water-cylinder/top$'
check "local: runs commands locally" [ -s "$FAKE_STATE/local" ]
check "local: names no host" not says 'pi@|on $'

# --local with no config anywhere: still lists, positions unknown.
fresh "$FULL"
PROBES_CONFIGS="$WORK/none.yaml $WORK/nor.yaml" runlocal
check "local, no config: ok" rc_is 0
check "local, no config: lists" says '28-00001172de60 +16\.250 °C +not in config$'
check "local, no config: says so" says 'no config found'

fresh "$EMPTY $ONE"
PROBES_CONFIGS="$WORK/config.yaml" runlocal --watch --interval 0 --count 2
check "local watch: arrival" says '\+ 28-00001172de60 on w1_bus_master1: 16\.250 °C \(hot-water-cylinder/top\)'

fresh "$FULL"
runlocal --check --config "$WORK/config.yaml"
check "local check: counts" says '^ok: 2 buses, 10 probes on this machine$'
run --local --host pi@example
check "local and host: refused" rc_is 1

# --check and refusals.
fresh "$FULL"
run --check
check "check: ok" rc_is 0
check "check: counts" says 'ok: 2 buses, 10 probes on pi@example'
check "check: reads no temperatures" not grep -q w1_slave "$FAKE_STATE/log"

fresh "$FULL"; export FAKE_SSH_FAIL=1
run
check "no ssh: refused" rc_is 1
check "no ssh: says so" says "can't ssh"

fresh "$WORK/nowhere"
run
check "no buses: refused" rc_is 1
check "no buses: says so" says "no 1-Wire bus masters"

fresh "$FULL"
run --config "$WORK/missing.yaml"
check "missing config: refused" rc_is 1
run --interval x --watch
check "bad interval: refused" rc_is 1
run --bogus
check "unknown option: refused" rc_is 1
run --help
check "help" says "Usage: deploy/probes.sh"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
