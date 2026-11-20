#!/usr/bin/env bash
# shellcheck disable=SC2015 # "test && ok || bad": ok always succeeds, so bad runs only on a failed test
# Tests for deploy/move-topics.sh. mosquitto_sub, mosquitto_pub and setup-pi.sh
# are stubs; the "broker" is a directory holding one file per retained topic.
# No real broker or Pi is touched.
#
# Usage: ./deploy/move-topics_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/move-topics.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/move-topics-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"
mkdir -p "$STUBS"
export BROKER_DIR="$WORK/broker" FAKE_LOG="$WORK/log"

# Retained topics are files named with / as %2F.
cat >"$STUBS/mosquitto_sub" <<'EOF'
#!/usr/bin/env bash
echo "mosquitto_sub $*" >>"$FAKE_LOG"
filters=()
while [ $# -gt 0 ]; do
    case "$1" in -t) filters+=("$2"); shift ;; -h | -p | -W | -F) shift ;; esac
    shift
done
for f in "$BROKER_DIR"/*; do
    [ -e "$f" ] || continue
    t=$(basename "$f" | sed 's#%2F#/#g')
    for flt in "${filters[@]}"; do
        base=${flt%/#}
        if [ "$flt" = "$t" ] || { [ "$base" != "$flt" ] && { [ "$t" = "$base" ] || [ "${t#"$base"/}" != "$t" ]; }; }; then
            echo "$t $(cat "$f")"
        fi
    done
done
exit 27 # what -W does when it times out
EOF
cat >"$STUBS/mosquitto_pub" <<'EOF'
#!/usr/bin/env bash
echo "mosquitto_pub $*" >>"$FAKE_LOG"
t="" empty=0 retain=0
while [ $# -gt 0 ]; do
    case "$1" in -t) t="$2"; shift ;; -n) empty=1 ;; -r) retain=1 ;; -h | -p) shift ;; esac
    shift
done
[ "$retain" = 1 ] && [ "$empty" = 1 ] && rm -f "$BROKER_DIR/$(sed 's#/#%2F#g' <<<"$t")"
EOF
# setup-pi.sh: logs its args; unless --check, "restarts" tanktemp onto the
# config's prefix: the old status becomes SHUTDOWN, the new topics appear.
# FAKE_SETUP_FAIL=1 fails; FAKE_NO_PUBLISH=1 restarts but publishes nothing.
cat >"$STUBS/setup-pi" <<'EOF'
#!/usr/bin/env bash
echo "setup-pi $*" >>"$FAKE_LOG"
cfg="" check=0
while [ $# -gt 0 ]; do
    case "$1" in --config) cfg="$2"; shift ;; --check) check=1 ;; esac
    shift
done
grep -q "topic_prefix:" "$cfg" && echo "setup-pi saw prefix $(sed -n 's/^ *topic_prefix: *\([^ ]*\).*/\1/p' "$cfg")" >>"$FAKE_LOG"
[ -z "${FAKE_SETUP_FAIL:-}" ] || exit 1
[ "$check" = 0 ] || exit 0
[ -z "${FAKE_NO_PUBLISH:-}" ] || exit 0
p=$(sed -n 's/^ *topic_prefix: *\([^ ]*\).*/\1/p' "$cfg")
put() { printf '%s' "$2" >"$BROKER_DIR/$(sed 's#/#%2F#g' <<<"$1")"; }
for f in "$BROKER_DIR"/*status; do
    [ -e "$f" ] && printf '%s' '{"online":false,"event":"SHUTDOWN"}' >"$f"
done
put "$p/status" '{"online":true,"event":"HEARTBEAT","ready":true}'
for id in $(sed -n 's/^ *- device_id: *//p' "$cfg"); do put "$p/$id/state" '{"device_id":"'"$id"'"}'; done
EOF
chmod +x "$STUBS"/*

export PATH="$STUBS:$PATH" MOVE_TOPICS_SETUP_PI="$STUBS/setup-pi"
export MOVE_TOPICS_VERIFY_TIMEOUT=0 MOVE_TOPICS_VERIFY_EVERY=0 MOVE_TOPICS_LISTEN_S=1

CFG="$WORK/config.yaml"
write_config() {
    cat >"$CFG" <<EOF
sweep: {interval: 30s}
mqtt:
  broker: tcp://192.0.2.7:1883
  topic_prefix: ${1:-energy/hot_water}   # a comment kept
tanks:
  # a comment
  - device_id: left_tank
    sensors: []
  - device_id: right_tank
    sensors: []
EOF
}
put() { printf '%s' "$2" >"$BROKER_DIR/${1//\//%2F}"; }
has() { [ -e "$BROKER_DIR/${1//\//%2F}" ]; }
# The broker as production has it before the move.
reset_world() {
    rm -rf "$BROKER_DIR"; mkdir -p "$BROKER_DIR"
    write_config "${1:-}"
    put energy/hot_water/status '{"online":true,"event":"HEARTBEAT"}'
    put energy/hot_water/left_tank/state '{}'
    put energy/hot_water/right_tank/state '{}'
    put energy/boiler/sensor/events '{"x":1}' # someone else's, never touched
}

PASS=0 FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   $1"; }
# shellcheck disable=SC2001 # indents every line of a multi-line string
bad() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -z "${2:-}" ] || sed 's/^/     /' <<<"$2"; }
run() {
    local name=$1 want=$2 got=0
    shift 2
    : >"$FAKE_LOG"
    OUT_TXT=$("$SCRIPT" --config "$CFG" "$@" 2>&1 </dev/null) || got=$?
    if [ "$got" = "$want" ]; then ok "$name (exit $got)"; else bad "$name: exit $got, want $want" "$OUT_TXT"; fi
}
expect_out() { if grep -q -- "$2" <<<"$OUT_TXT"; then ok "$1"; else bad "$1: no '$2' in output" "$OUT_TXT"; fi; }
expect_log() { if grep -q -- "$2" "$FAKE_LOG"; then ok "$1"; else bad "$1: '$2' not in log" "$(cat "$FAKE_LOG")"; fi; }
expect_no_log() { if grep -q -- "$2" "$FAKE_LOG"; then bad "$1: '$2' in log" "$(cat "$FAKE_LOG")"; else ok "$1"; fi; }
prefix_is() { if grep -qE "^  topic_prefix: $2   # a comment kept$" "$CFG"; then ok "$1"; else bad "$1" "$(cat "$CFG")"; fi; }

# --- --check changes nothing ----------------------------------------------
reset_world
cp "$CFG" "$WORK/before.yaml"
run "--check" 0 --check
cmp -s "$CFG" "$WORK/before.yaml" && ok "--check: local config untouched" || bad "--check changed the config"
expect_log "--check: setup-pi --check" "setup-pi --config .* --check"
expect_log "--check: setup-pi checked the new prefix (on a copy)" "setup-pi saw prefix water/hot/tanktemp"
expect_no_log "--check: nothing published" "mosquitto_pub"
expect_out "--check: says it would change the prefix" "would change topic_prefix: energy/hot_water -> water/hot/tanktemp"
expect_out "--check: says not live yet" "not live on water/hot/tanktemp yet"
has energy/hot_water/status && ok "--check: old retained kept" || bad "--check removed old retained"
run "--check --clear-old" 0 --check --clear-old
expect_out "--check --clear-old: says what it would clear" "would clear these 3"
expect_no_log "--check --clear-old: nothing published" "mosquitto_pub"

# --- the first run ---------------------------------------------------------
reset_world
run "move" 0 --yes
prefix_is "move: local prefix changed, comment and indent kept" water/hot/tanktemp
grep -q "^  - device_id: left_tank" "$CFG" && grep -q "^sweep: {interval: 30s}" "$CFG" && ok "move: rest of the config untouched" || bad "move: config damaged" "$(cat "$CFG")"
expect_log "move: setup-pi --yes on the real config" "setup-pi --config $CFG --yes"
expect_out "move: verified live" "live on water/hot/tanktemp: status online, state for left_tank right_tank"
has energy/hot_water/status && has energy/hot_water/left_tank/state && ok "move: old retained left for consumers" || bad "move: old retained cleared early"
expect_out "move: says the next step" "move-topics.sh --clear-old"
expect_no_log "move: nothing cleared" "mosquitto_pub"

run "move again is a no-op" 0 --yes
expect_out "again: already moved" "already publishes to water/hot/tanktemp"
prefix_is "again: prefix still new" water/hot/tanktemp

# --- clearing ----------------------------------------------------------------
run "--clear-old, unanswered, stops" 1 --clear-old
has energy/hot_water/status && ok "unanswered: nothing cleared" || bad "unanswered: cleared"

run "--clear-old --yes" 0 --clear-old --yes
for t in energy/hot_water/status energy/hot_water/left_tank/state energy/hot_water/right_tank/state; do
    has "$t" && bad "clear: $t still retained" || ok "clear: $t cleared"
done
expect_log "clear: zero-length retained publish" "mosquitto_pub -h 192.0.2.7 -p 1883 -r -n -t energy/hot_water/status"
has water/hot/tanktemp/status && has water/hot/tanktemp/left_tank/state && ok "clear: new topics kept" || bad "clear: new topics lost"
has energy/boiler/sensor/events && ok "clear: other publishers' topics kept" || bad "clear: touched energy/boiler"
run "--clear-old again is a no-op" 0 --clear-old --yes
expect_out "clear again: nothing to clear" "none: nothing to clear"

# --- refusals ----------------------------------------------------------------
reset_world
FAKE_NO_PUBLISH=1 run "not live after setup: fails" 1 --yes --clear-old
expect_out "not live: says what's missing" "still missing: water/hot/tanktemp/status (online)"
has energy/hot_water/status && ok "not live: old retained kept" || bad "not live: cleared anyway"

reset_world
put water/hot/tanktemp/status '{"online":false,"event":"LWT"}'
put water/hot/tanktemp/left_tank/state '{}'
put water/hot/tanktemp/right_tank/state '{}'
FAKE_NO_PUBLISH=1 run "offline status is not live" 1 --yes --clear-old
has energy/hot_water/status && ok "offline: old retained kept" || bad "offline: cleared anyway"

reset_world
FAKE_SETUP_FAIL=1 run "setup-pi fails: stops" 1 --yes --clear-old
expect_out "setup-pi fails: says so" "setup-pi.sh stopped"
expect_no_log "setup-pi fails: nothing published" "mosquitto_pub"

reset_world other/prefix
run "unknown prefix refused" 1 --yes
grep -q "topic_prefix: other/prefix" "$CFG" && ok "unknown prefix: config untouched" || bad "unknown prefix: config changed"
expect_no_log "unknown prefix: setup-pi not run" "setup-pi"

reset_world
sed -i.bak '/topic_prefix/d' "$CFG"
run "no topic_prefix line refused" 1 --yes
expect_out "no prefix: says why" "want exactly 1"

reset_world
run "--to inside --from refused" 1 --yes --to energy/hot_water/new
run "--from inside --to refused" 1 --yes --from water/hot/tanktemp/x
run "wildcard prefix refused" 1 --yes --to 'water/#'
run "same prefixes refused" 1 --yes --to energy/hot_water

reset_world
printf 'mqtt: {broker: tcp://192.0.2.7:1883, topic_prefix: energy/hot_water}\ntanks:\n  - device_id: a\n' >"$CFG"
run "flow-style mqtt refused" 1 --yes

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
