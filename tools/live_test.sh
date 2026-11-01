#!/usr/bin/env bash
# shellcheck disable=SC2016 # the stubs below expand their own $ variables
# Tests for tools/live.sh, against fixture configs and stub nc/open. No
# network, no browser.
#
# Usage: ./tools/live_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
# nc succeeds unless NC_FAIL is set; open records its argument. The $ in
# these stubs is for the stub to expand, not this script.
printf '#!/usr/bin/env bash\n[ -z "${NC_FAIL:-}" ]\n' >"$WORK/bin/nc"
printf '#!/usr/bin/env bash\necho "$1" >"%s/opened"\n' "$WORK" >"$WORK/bin/open"
chmod +x "$WORK/bin/"*

pass=0 fail=0
check() {
    local name="$1"; shift
    if "$@"; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $name"; echo "  output: $OUT"; fi
}
run() { set +e; OUT=$(PATH="$WORK/bin:$PATH" "$HERE/live.sh" "$@" 2>&1); RC=$?; set -e; }
says() { grep -qE -- "$1" <<<"$OUT"; }
rc_is() { [ "$RC" -eq "$1" ]; }
not() { ! "$@"; }

cat >"$WORK/block.yaml" <<'EOF'
mqtt:
  broker: tcp://broker.example:1883
  topic_prefix: test/energy/hot_water
tanks: []
EOF
cat >"$WORK/flow.yaml" <<'EOF'
mqtt: {broker: tcp://192.0.2.9:1883, client_id: x}
tanks: []
EOF
cat >"$WORK/commented.yaml" <<'EOF'
mqtt:
  broker: tcp://broker.example:1883
  # topic_prefix: test/energy/hot_water
EOF
printf 'tanks: []\n' >"$WORK/nobroker.yaml"

run --check --config "$WORK/block.yaml"
check "block style: broker and prefix" rc_is 0
check "block style: url" says "live.html#ws=ws://broker.example:9001&prefix=test/energy/hot_water$"
check "block style: probe ok" says "^ok: broker.example:9001"

run --check --config "$WORK/flow.yaml" --ws-port 8083
check "flow style: default prefix, custom port" says "#ws=ws://192.0.2.9:8083&prefix=energy/hot_water$"

run --check --config "$WORK/commented.yaml"
check "a commented-out prefix is ignored" says "prefix=energy/hot_water$"

NC_FAIL=1 run --check --config "$WORK/block.yaml"
check "unreachable port fails" rc_is 1
check "unreachable port says why" says "websockets listener"

run --check --config "$WORK/nobroker.yaml"
check "no broker fails" rc_is 1
check "no broker says so" says "no mqtt.broker"

run --check --config "$WORK/missing.yaml"
check "missing config fails" rc_is 1

run --ws-port abc --config "$WORK/block.yaml"
check "bad port refused" rc_is 1

run --bogus
check "unknown option refused" rc_is 1

run --check --demo --config "$WORK/missing.yaml"
check "demo needs no config" rc_is 0
check "demo url" says "live.html#demo=1$"

# macOS `open` drops the #fragment from a file:// URL, so live.sh opens a
# small launcher page that redirects to the full URL.
rm -f "$WORK/opened"
TMPDIR="$WORK" run --config "$WORK/block.yaml"
check "opens a launcher, not the bare URL" not grep -q "#" "$WORK/opened"
check "the launcher redirects to the full URL" grep -q "live.html#ws=ws://broker.example:9001&prefix=test/energy/hot_water" "$(cat "$WORK/opened" 2>/dev/null)"

rm -f "$WORK/opened"
run --check --config "$WORK/block.yaml"
check "--check opens nothing" test ! -e "$WORK/opened"

run --help
check "help" says "Usage: ./tools/live.sh"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
