#!/usr/bin/env bash
# Open tools/live.html, the live MQTT view, pointed at the broker and topic
# prefix in a tanktemp config. The page connects over MQTT-over-WebSockets,
# so the broker needs a websockets listener (mosquitto: `listener 9001` +
# `protocol websockets`). Read-only: it subscribes and never publishes.
#
# Usage: ./tools/live.sh [--check] [--demo] [--config FILE] [--ws-port N]
#   --check     print the URL and probe the websocket port; open nothing
#   --demo      open the page on its made-up demo, ignoring the config
#   --config    the tanktemp config to read (default deploy/config.local.yaml)
#   --ws-port   the broker's websocket port (default 9001)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
PAGE="$HERE/live.html"
CONFIG="$ROOT/deploy/config.local.yaml"
WS_PORT=9001 CHECK=0 DEMO=0

die() { echo "live.sh: $*" >&2; exit 1; }
usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --demo) DEMO=1 ;;
        --config) [ $# -ge 2 ] || die "--config needs a file"; CONFIG="$2"; shift ;;
        --ws-port) [ $# -ge 2 ] || die "--ws-port needs a number"; WS_PORT="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option $1 (try --help)" ;;
    esac
    shift
done

[[ "$WS_PORT" =~ ^[0-9]+$ ]] || die "--ws-port must be a number, got '$WS_PORT'"
[ -f "$PAGE" ] || die "missing $PAGE"

if [ "$DEMO" -eq 1 ]; then
    URL="file://$PAGE#demo=1"
else
    [ -f "$CONFIG" ] || die "no config at $CONFIG (pass --config, or --demo)"
    # The first broker: and topic_prefix: values, in block or flow style.
    broker=$(grep -oE 'broker:[[:space:]]*[^[:space:],}#]+' "$CONFIG" | head -1 | sed -E 's/broker:[[:space:]]*//') || true
    prefix=$(grep -vE '^[[:space:]]*#' "$CONFIG" | grep -oE 'topic_prefix:[[:space:]]*[^[:space:],}#]+' | head -1 | sed -E 's/topic_prefix:[[:space:]]*//') || true
    [ -n "$broker" ] || die "no mqtt.broker in $CONFIG"
    prefix=${prefix:-energy/hot_water}
    host=$(sed -E 's#^[a-z]+://##; s#:[0-9]+$##' <<<"$broker")
    [ -n "$host" ] || die "can't read a host from broker '$broker'"
    URL="file://$PAGE#ws=ws://$host:$WS_PORT&prefix=$prefix"
fi

if [ "$CHECK" -eq 1 ]; then
    echo "$URL"
    if [ "$DEMO" -eq 0 ]; then
        if nc -z -G 3 "$host" "$WS_PORT" >/dev/null 2>&1 || nc -z -w 3 "$host" "$WS_PORT" >/dev/null 2>&1; then
            echo "ok: $host:$WS_PORT is accepting connections"
        else
            die "$host:$WS_PORT isn't reachable; is the broker's websockets listener on?"
        fi
    fi
    exit 0
fi

# macOS `open` drops the #fragment from a file:// URL, which carries the
# broker, so open a launcher page that redirects to the full URL.
LAUNCHER="${TMPDIR:-/tmp}"
LAUNCHER="${LAUNCHER%/}/tanktemp-live-open.html"
printf '<!doctype html><meta charset="utf-8"><title>tanktemp live</title><script>location.replace(%s)</script><a href="%s">Open tanktemp live</a>\n' \
    "\"$URL\"" "$URL" >"$LAUNCHER"
echo "Opening $URL"
if command -v open >/dev/null 2>&1; then open "$LAUNCHER"
elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$LAUNCHER"
else echo "Open this in a browser: $URL"; fi
