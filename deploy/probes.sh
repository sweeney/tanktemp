#!/usr/bin/env bash
# What's on the Pi's 1-Wire buses right now: every DS18B20, its bus, its
# temperature, and where the local config puts it. With --watch, a line each
# time a probe arrives or leaves, for fitting probes one or two at a time.
# When two arrive together, hold one in your hand: watch names the one that
# warms. Read-only on the Pi: it lists the buses and reads w1_slave (a fresh
# conversion), nothing else.
#
# Usage: deploy/probes.sh [--host user@host] [--config FILE] [--watch]
#   --watch       report arrivals and departures until Ctrl-C
#   --interval S  seconds between polls in --watch (default 3)
#   --count N     stop --watch after N polls (default: never)
#   --check       check the Pi answers and has buses; read no temperatures
#   --config      the config whose tanks and positions to show
#                 (default deploy/config.local.yaml)
#   --host        ssh target (default: the contents of deploy/host)
#
# The kernel searches each bus about every 10 s, so a probe appears up to
# ~10 s after it's connected, and leaves only after a few failed searches.
set -euo pipefail
[ "${BASH_VERSINFO[0]}" -ge 4 ] || { echo "probes.sh: needs bash 4 or later (this is $BASH_VERSION); try: brew install bash" >&2; exit 1; }

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
CONFIG="$ROOT/deploy/config.local.yaml"
HOST="" WATCH=0 CHECK=0 INTERVAL=3 COUNT=0
W1=/sys/bus/w1/devices
WARM_C=0.5 # how much a held probe must warm to be named

die() { echo "probes.sh: $*" >&2; exit 1; }
usage() { sed -n '2,/^$/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
    case "$1" in
        --watch) WATCH=1 ;;
        --check) CHECK=1 ;;
        --interval) [ $# -ge 2 ] || die "--interval needs seconds"; INTERVAL="$2"; shift ;;
        --count) [ $# -ge 2 ] || die "--count needs a number"; COUNT="$2"; shift ;;
        --config) [ $# -ge 2 ] || die "--config needs a file"; CONFIG="$2"; shift ;;
        --host) [ $# -ge 2 ] || die "--host needs user@host"; HOST="$2"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option $1 (try --help)" ;;
    esac
    shift
done
[[ "$INTERVAL" =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "--interval must be a number of seconds"
[[ "$COUNT" =~ ^[0-9]+$ ]] || die "--count must be a number"
[ -f "$CONFIG" ] || die "no config at $CONFIG (pass --config)"
if [ -z "$HOST" ]; then
    [ -f "$ROOT/deploy/host" ] || die "no --host and no deploy/host file"
    HOST=$(tr -d '[:space:]' <"$ROOT/deploy/host")
fi

# rom -> tank/position, from the config, in flow or block style.
declare -A WHERE=()
while read -r rom where; do WHERE[$rom]=$where; done < <(awk '
    match($0, /device_id: *[a-z0-9_-]+/) { tank = substr($0, RSTART, RLENGTH); sub(/device_id: */, "", tank) }
    match($0, /rom: *28-[0-9a-f]+/) { cur = substr($0, RSTART, RLENGTH); sub(/rom: */, "", cur) }
    match($0, /position: *[a-z0-9_]+/) { p = substr($0, RSTART, RLENGTH); sub(/position: */, "", p); if (cur != "") print cur, tank "/" p; cur = "" }
' "$CONFIG")
where() { echo "${WHERE[$1]:-not in config}"; }

# One line per bus ("BUS") and per probe ("BUS ROM").
presence() {
    # shellcheck disable=SC2016 # expanded on the Pi
    ssh "$HOST" 'for m in '"$W1"'/w1_bus_master*; do [ -d "$m" ] || continue; b=${m##*/}; echo "$b"; grep "^28-" "$m/w1_master_slaves" | sed "s/^/$b /"; done'
}

# "ROM KIND MILLI" per ROM: KIND is ok, crc, bus_low, power_on_reset or
# missing, decided from the w1_slave bytes as in docs/ONEWIRE.md.
temps() {
    [ $# -gt 0 ] || return 0
    # shellcheck disable=SC2016 # expanded on the Pi
    ssh "$HOST" 'for r in '"$*"'; do f='"$W1"'/$r/w1_slave; if [ -r "$f" ]; then echo "$r $(tr "\n" " " <"$f")" & else echo "$r MISSING"; fi; done; wait' |
        awk '{
            if ($2 == "MISSING" || NF < 23) { print $1, "missing", "-"; next }
            zeros = 1; for (i = 2; i <= 10; i++) if ($i != "00") zeros = 0
            t = $NF; sub(/t=/, "", t)
            if ($13 != "YES") k = "crc"
            else if (zeros) k = "bus_low"
            else if ($2 == "50" && $3 == "05" && $8 == "0c") k = "power_on_reset"
            else k = "ok"
            print $1, k, t
        }'
}
show() { # KIND MILLI -> what to print
    if [ "$1" = ok ]; then awk -v t="$2" 'BEGIN { printf "%.3f °C", t / 1000 }'; else echo "$1"; fi
}

state=$(presence) || die "can't ssh to $HOST"
buses=$(grep -c '^[^ ][^ ]*$' <<<"$state" || true)
[ "$buses" -gt 0 ] || die "$HOST has no 1-Wire bus masters"
probes=$(grep -c ' ' <<<"$state" || true)

if [ "$CHECK" -eq 1 ]; then
    echo "ok: $buses buses, $probes probes on $HOST"
    exit 0
fi

if [ "$WATCH" -eq 0 ]; then
    declare -A T=()
    # shellcheck disable=SC2046 # one ROM per word
    while read -r rom kind milli; do T[$rom]=$(show "$kind" "$milli"); done < <(temps $(awk 'NF == 2 { print $2 }' <<<"$state"))
    configured=0
    while read -r bus rom; do
        if [ -z "$rom" ]; then
            grep -q "^$bus " <<<"$state" || printf '%-15s (no probes)\n' "$bus"
            continue
        fi
        [ "$(where "$rom")" = "not in config" ] || configured=$((configured + 1))
        printf '%-15s %-16s %-10s  %s\n' "$bus" "$rom" "${T[$rom]:-missing}" "$(where "$rom")"
    done <<<"$state"
    echo "$probes probes on $buses buses, $configured in config"
    exit 0
fi

# --watch.
summary=$(awk 'NF == 1 { order[++n] = $1; c[$1] += 0 } NF == 2 { c[$1]++ } END { for (i = 1; i <= n; i++) printf "%s%d on %s", (i > 1 ? ", " : ""), c[order[i]], order[i] }' <<<"$state")
echo "watching $buses buses: $summary. Connect probes; Ctrl-C to stop."
declare -A BASE=() # ROMs that arrived together, and their temperature then
prev=$state polls=1
now() { date +%H:%M:%S; }
while [ "$COUNT" -eq 0 ] || [ "$polls" -lt "$COUNT" ]; do
    sleep "$INTERVAL"
    cur=$(presence) || { echo "$(now) can't reach $HOST; retrying" >&2; continue; }
    polls=$((polls + 1))
    arrived=$(comm -13 <(awk 'NF == 2' <<<"$prev" | sort) <(awk 'NF == 2' <<<"$cur" | sort))
    left=$(comm -23 <(awk 'NF == 2' <<<"$prev" | sort) <(awk 'NF == 2' <<<"$cur" | sort))
    while read -r bus rom; do
        [ -n "$rom" ] || continue
        echo "$(now) - $rom left $bus"
        unset "BASE[$rom]"
    done <<<"$left"

    # Probes still to tell apart: whichever has warmed is in your hand.
    if [ ${#BASE[@]} -gt 0 ]; then
        while read -r rom kind milli; do
            [ "$kind" = ok ] && [ -n "${BASE[$rom]:-}" ] || continue
            rise=$(awk -v a="$milli" -v b="${BASE[$rom]}" 'BEGIN { printf "%.1f", (a - b) / 1000 }')
            if awk -v r="$rise" -v w="$WARM_C" 'BEGIN { exit !(r >= w) }'; then
                echo "$(now) ↑ $rom is warming (+$rise °C): it's the one in your hand ($(where "$rom"))"
                BASE=()
                break
            fi
        done < <(temps "${!BASE[@]}")
    fi

    if [ -n "$arrived" ]; then
        declare -A BUS=()
        while read -r bus rom; do BUS[$rom]=$bus; done <<<"$arrived"
        n=0
        while read -r rom kind milli; do
            n=$((n + 1))
            echo "$(now) + $rom on ${BUS[$rom]}: $(show "$kind" "$milli") ($(where "$rom"))"
            [ "$kind" != ok ] || BASE[$rom]=$milli
        done < <(temps "${!BUS[@]}")
        if [ "$n" -gt 1 ]; then
            echo "$(now) $n arrived together: hold ONE in your hand for a minute and I'll say which warms."
        else
            BASE=()
        fi
        unset BUS
    fi
    prev=$cur
done
