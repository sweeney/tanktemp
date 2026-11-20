#!/usr/bin/env bash
# Move tanktemp's MQTT topics to a new prefix (default: energy/hot_water ->
# water/hot/tanktemp), as a clean cut with no dual-publishing, in two runs:
#
#   1. ./deploy/move-topics.sh [--check] [--yes] [user@host]
#      - sets mqtt.topic_prefix in the local config (deploy/config.local.yaml)
#      - runs ./setup-pi.sh with it, which shows the diff, installs it as
#        /etc/tanktemp/config.yaml and restarts tanktemp onto it (asking
#        first, unless --yes)
#      - checks the broker has a retained status (online) and a retained
#        state for every tank on the new prefix
#   Then consumers move (statehouse: its base_topic, then a restart).
#   2. ./deploy/move-topics.sh --clear-old [--check] [--yes] [user@host]
#      - does everything in 1 again (each step is a no-op once done)
#      - clears every retained message left under the old prefix with a
#        zero-length retained publish, asking first unless --yes
#
# Clearing waits for the second run because a consumer still subscribed to
# the old topics receives the clears as empty messages.
#
# Usage: ./deploy/move-topics.sh [--check] [--yes] [--clear-old]
#                                [--from PREFIX] [--to PREFIX] [--config FILE] [user@host]
#   --check      say what would happen; change nothing (setup-pi --check on a copy)
#   --yes        don't ask (passed on to setup-pi.sh)
#   --clear-old  also clear the retained messages under the old prefix
#   --from, --to the old and new prefixes (defaults above)
#   --config     the local config (default deploy/config.local.yaml)
#   user@host    passed to setup-pi.sh (default: its own, deploy/host)
#
# Needs mosquitto_sub and mosquitto_pub. Re-running is safe at any point.
# Exit: 0 done (or --check passed), 1 refused or failed (nothing further done).
# Tests: ./deploy/move-topics_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
SETUP_PI="${MOVE_TOPICS_SETUP_PI:-$ROOT/setup-pi.sh}"
VERIFY_TIMEOUT="${MOVE_TOPICS_VERIFY_TIMEOUT:-120}"
VERIFY_EVERY="${MOVE_TOPICS_VERIFY_EVERY:-5}"
LISTEN_S="${MOVE_TOPICS_LISTEN_S:-3}"

FROM=energy/hot_water TO=water/hot/tanktemp
CONFIG="$ROOT/deploy/config.local.yaml"
CHECK=0 YES=0 CLEAR=0 HOST=""

die() { echo "move-topics: $*" >&2; exit 1; }
info() { echo "move-topics: $*"; }
step() { echo; echo "==> $*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check | --dry-run) CHECK=1 ;;
        --yes) YES=1 ;;
        --clear-old) CLEAR=1 ;;
        --from) [ $# -ge 2 ] || die "--from needs a prefix"; FROM="$2"; shift ;;
        --to) [ $# -ge 2 ] || die "--to needs a prefix"; TO="$2"; shift ;;
        --config) [ $# -ge 2 ] || die "--config needs a file"; CONFIG="$2"; shift ;;
        -h | --help) sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*) die "unknown option $1 (try --help)" ;;
        *) [ -z "$HOST" ] || die "only one host, please"; HOST="$1" ;;
    esac
    shift
done

# --- Preconditions (read-only) ---------------------------------------------
plain='^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$'
[[ "$FROM" =~ $plain ]] || die "--from '$FROM' is not a plain topic path"
[[ "$TO" =~ $plain ]] || die "--to '$TO' is not a plain topic path"
[ "$FROM" != "$TO" ] || die "--from and --to are the same"
case "$TO/" in "$FROM/"*) die "$TO is inside $FROM: clearing $FROM/# would clear the new topics too" ;; esac
case "$FROM/" in "$TO/"*) die "$FROM is inside $TO: the old and new trees overlap" ;; esac
for t in mosquitto_sub mosquitto_pub; do
    command -v "$t" >/dev/null || die "$t not found (brew install mosquitto)"
done
[ -x "$SETUP_PI" ] || die "$SETUP_PI not found"
[ -r "$CONFIG" ] || die "$CONFIG not found"

# Active (uncommented) lines only.
active=$(grep -vE '^[[:space:]]*#' "$CONFIG")
grep -qE '^mqtt:[[:space:]]*\{' <<<"$active" && die "$CONFIG writes mqtt: in flow style ({...}); make it block style first"
n=$(grep -cE '^[[:space:]]+topic_prefix:' <<<"$active" || true)
[ "$n" = 1 ] || die "$CONFIG has $n active topic_prefix lines; want exactly 1 under mqtt: (set it explicitly: the running build's default is the old prefix)"
PREFIX=$(sed -nE 's/^[[:space:]]+topic_prefix:[[:space:]]*([^[:space:]#]+).*/\1/p' <<<"$active")
BROKER=$(sed -nE 's/^[[:space:]]+broker:[[:space:]]*([^[:space:]#]+).*/\1/p' <<<"$active" | head -1)
[[ "$BROKER" =~ ^tcp://([^:/]+):([0-9]+)$ ]] || die "cannot read mqtt.broker ('$BROKER') as tcp://host:port"
BHOST=${BASH_REMATCH[1]} BPORT=${BASH_REMATCH[2]}
IDS=()
while read -r id; do [ -z "$id" ] || IDS+=("$id"); done < <(sed -nE 's/^[[:space:]]*-[[:space:]]*device_id:[[:space:]]*([A-Za-z0-9_-]+).*/\1/p' <<<"$active")
[ ${#IDS[@]} -gt 0 ] || die "$CONFIG has no tanks (no device_id lines)"
case "$PREFIX" in
    "$FROM" | "$TO") ;;
    *) die "$CONFIG publishes to '$PREFIX', neither $FROM nor $TO; refusing to guess" ;;
esac

# retained FILTER: "topic payload" for each retained message matching FILTER.
retained() {
    mosquitto_sub -h "$BHOST" -p "$BPORT" -t "$1" --retained-only -W "$LISTEN_S" -F '%t %p' 2>/dev/null || true
}
# live_on PREFIX: the status says online and every tank has a state.
MISSING=""
live_on() {
    MISSING=""
    local got
    got=$(retained "$1/#")
    grep -qE "^$1/status [{].*\"online\":true" <<<"$got" || MISSING="$1/status (online)"
    for id in "${IDS[@]}"; do
        grep -q "^$1/$id/state " <<<"$got" || MISSING="${MISSING:+$MISSING, }$1/$id/state"
    done
    [ -z "$MISSING" ]
}

info "broker $BHOST:$BPORT, tanks: ${IDS[*]}"
info "moving $FROM -> $TO"

# --- 1. The local config ----------------------------------------------------
step "1. $CONFIG"
USE_CONFIG="$CONFIG"
if [ "$PREFIX" = "$TO" ]; then
    info "already publishes to $TO"
elif [ "$CHECK" = 1 ]; then
    USE_CONFIG=$(mktemp "${TMPDIR:-/tmp}/move-topics.XXXXXX")
    trap 'rm -f "$USE_CONFIG"' EXIT
    sed -E "s#^([[:space:]]+topic_prefix:[[:space:]]*)$FROM([[:space:]]|\$)#\1$TO\2#" "$CONFIG" >"$USE_CONFIG"
    info "would change topic_prefix: $FROM -> $TO (checking setup-pi on a copy)"
else
    tmp="$CONFIG.move-topics.$$"
    sed -E "s#^([[:space:]]+topic_prefix:[[:space:]]*)$FROM([[:space:]]|\$)#\1$TO\2#" "$CONFIG" >"$tmp"
    if ! grep -qE "^[[:space:]]+topic_prefix:[[:space:]]*$TO([[:space:]]|\$)" "$tmp"; then
        rm -f "$tmp"; die "could not rewrite topic_prefix in $CONFIG; nothing changed"
    fi
    mv "$tmp" "$CONFIG"
    info "topic_prefix: $FROM -> $TO"
fi

# --- 2. The Pi --------------------------------------------------------------
step "2. the Pi: ./setup-pi.sh"
sargs=(--config "$USE_CONFIG")
[ "$CHECK" = 1 ] && sargs+=(--check)
[ "$YES" = 1 ] && sargs+=(--yes)
[ -z "$HOST" ] || sargs+=("$HOST")
if ! "$SETUP_PI" "${sargs[@]}"; then
    die "setup-pi.sh stopped; nothing on the broker was changed. Fix that, then re-run"
fi

# --- 3. The new topics ------------------------------------------------------
step "3. retained messages under $TO"
if [ "$CHECK" = 1 ]; then
    if live_on "$TO"; then info "already live on $TO"; else info "not live on $TO yet (missing: $MISSING); setup would restart onto it"; fi
else
    waited=0
    until live_on "$TO"; do
        [ "$waited" -lt "$VERIFY_TIMEOUT" ] || die "after ${VERIFY_TIMEOUT}s still missing: $MISSING. Old retained messages left alone"
        sleep "$VERIFY_EVERY"; waited=$((waited + VERIFY_EVERY))
    done
    info "live on $TO: status online, state for ${IDS[*]}"
fi

# --- 4. The old topics ------------------------------------------------------
step "4. retained messages under $FROM"
OLD=$(retained "$FROM/#" | cut -d' ' -f1)
if [ -z "$OLD" ]; then
    info "none: nothing to clear"
    exit 0
fi
info "still retained:"; while read -r t; do echo "      $t"; done <<<"$OLD"
if [ "$CLEAR" != 1 ]; then
    info "left alone. Once every consumer has moved to $TO (statehouse: base_topic, then a restart), run:"
    info "  ./deploy/move-topics.sh --clear-old${HOST:+ $HOST}"
    exit 0
fi
if [ "$CHECK" = 1 ]; then
    info "would clear these $(wc -l <<<"$OLD" | tr -d ' ') (zero-length retained publishes)"
    info "--check: nothing changed"
    exit 0
fi
live_on "$TO" || die "not live on $TO (missing: $MISSING); refusing to clear $FROM"
if [ "$YES" != 1 ]; then
    printf 'move-topics: clear these %s retained messages on %s:%s? [y/N] ' "$(wc -l <<<"$OLD" | tr -d ' ')" "$BHOST" "$BPORT"
    read -r answer || answer=""
    case "$answer" in y | Y | yes) ;; *) info "stopped; nothing cleared"; exit 1 ;; esac
fi
while read -r t; do
    mosquitto_pub -h "$BHOST" -p "$BPORT" -r -n -t "$t" || die "clearing $t failed"
    info "cleared $t"
done <<<"$OLD"
left=$(retained "$FROM/#")
[ -z "$left" ] || die "still retained after clearing: $(cut -d' ' -f1 <<<"$left" | tr '\n' ' ')"
info "done: nothing retained under $FROM; tanktemp is live on $TO"
