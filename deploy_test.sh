#!/usr/bin/env bash
# Tests for deploy.sh and setup-pi.sh. Each case runs the real script in a
# throwaway git repo against stubs: ssh runs the "remote" commands locally with
# HOME pointed at a fake Pi home, and curl, mosquitto_sub, rsync, go,
# systemctl, journalctl, sudo, install, chown, stat, id, useradd and ps are
# scripted from that fake Pi's state. A "build" is a small shell script that
# answers -version and -check-config the way tanktemp does. File owners are
# recorded in $FAKE_STATE/owners (the stubs cannot chown), and a build run as
# the tanktemp user, or a unit started as it, fails the way the Pi would on a
# file that user cannot read or a path through the 0700 home. No network, no Pi.
#
# Usage: ./deploy_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"         # on the local PATH
RSTUBS="$WORK/remote-stubs" # on the "remote" PATH
MKFAKEBIN="$WORK/mkfakebin"
mkdir -p "$STUBS" "$RSTUBS"

# ---------------------------------------------------------------- fake builds

# mkfakebin VERSION OUT [PAD]: a "tanktemp" stamped with VERSION, padded with
# PAD bytes of comment so it looks big enough to be real. Like tanktemp, it
# prints -check-config's answer on stderr. FAKE_REJECT_VERSION=<v>: the build
# stamped <v> rejects every config.
cat >"$MKFAKEBIN" <<'EOF'
#!/usr/bin/env bash
ver="$1" out="$2" pad="${3:-0}"
{
    echo '#!/usr/bin/env bash'
    echo "# FAKEBIN version=$ver"
    echo "ver=$ver"
    cat <<'BIN'
cfg=/etc/tanktemp/config.yaml check=0
while [ $# -gt 0 ]; do
    case "$1" in
        -version) echo "$ver"; exit 0 ;;
        -check-config) check=1 ;;
        -config) cfg="$2"; shift ;;
    esac
    shift
done
[ "$check" = 1 ] || { echo "fake tanktemp: only -version and -check-config" >&2; exit 1; }
who="${FAKE_RUN_AS:-}"
[ -n "$who" ] || { [ "${FAKE_AS_ROOT:-0}" = 1 ] && who=root || who=$(id -un); }
[ -z "${FAKE_STATE:-}" ] || echo "check-config $ver $cfg as $who" >>"$FAKE_STATE/checks"
# The Pi's home is 0700: only its owner can run a build from under it.
if [ "$who" = tanktemp ] && [ -n "${FAKE_HOME:-}" ]; then
    case "$0" in "$FAKE_HOME"/*) echo "bash: $0: Permission denied" >&2; exit 126 ;; esac
fi
[ -r "$cfg" ] || { echo "tanktemp: open $cfg: no such file or directory" >&2; exit 1; }
if [ -n "${RSTUBS:-}" ] && ! "$RSTUBS/fake_can_read" "$who" "$cfg"; then
    echo "tanktemp: open $cfg: permission denied" >&2
    exit 1
fi
if [ "${FAKE_REJECT_VERSION:-}" = "$ver" ] || grep -q '^INVALID' "$cfg"; then
    echo "tanktemp: $cfg: mqtt.broker: required" >&2
    exit 1
fi
broker=$(sed -n 's/^ *broker: *//p' "$cfg" | head -1)
prefix=$(sed -n 's/^ *topic_prefix: *//p' "$cfg" | head -1)
echo "$cfg: ok: 2 tanks, 10 sensors, publishing to ${prefix:-energy/hot_water} on $broker" >&2
exit 0
BIN
    [ "$pad" -eq 0 ] || head -c "$pad" /dev/zero | tr '\0' '#'
} >"$out"
chmod 0755 "$out"
EOF
chmod +x "$MKFAKEBIN"

# ---------------------------------------------------------------- local stubs

cat >"$STUBS/ssh" <<'EOF'
#!/usr/bin/env bash
# ssh [-n] [-o opt]... host [command...]: run the command "on the Pi".
while [ $# -gt 0 ]; do
    case "$1" in
        -n) shift ;;
        -o) shift 2 ;;
        *) break ;;
    esac
done
echo "$1" >>"$FAKE_STATE/ssh_hosts"
shift # host
[ "${FAKE_SSH_DOWN:-0}" = 1 ] && { echo "ssh: connect: timed out" >&2; exit 255; }
if [ -f "$FAKE_STATE/ssh_down" ]; then
    [ "$(cat "$FAKE_STATE/ssh_down")" = once ] && rm "$FAKE_STATE/ssh_down"
    echo "ssh: connect: no route to host" >&2
    exit 255
fi
cd "$FAKE_HOME"
HOME="$FAKE_HOME" PATH="$RSTUBS:$PATH" exec bash -c "$*"
EOF

cat >"$STUBS/rsync" <<'EOF'
#!/usr/bin/env bash
[ "${FAKE_RSYNC_FAIL:-0}" = 1 ] && exit 12
src="${*: -2:1}" dest="${*: -1}" path="${*: -1}"
path="${path#*:}"
case "$path" in /*) ;; *) path="$FAKE_HOME/$path" ;; esac
cp "$src" "$path"
case "$dest" in *.incoming-*) [ -n "${FAKE_SSH_DROP_AFTER_UPLOAD:-}" ] && echo "$FAKE_SSH_DROP_AFTER_UPLOAD" >"$FAKE_STATE/ssh_down" ;; esac
true
EOF

cat >"$STUBS/go" <<'EOF'
#!/usr/bin/env bash
echo "go $* GOOS=${GOOS:-} GOARCH=${GOARCH:-} GOARM=${GOARM:-} CGO_ENABLED=${CGO_ENABLED:-} GOWORK=${GOWORK:-}" >>"$FAKE_STATE/go"
case "$1" in
    vet) exit "${FAKE_VET_RC:-0}" ;;
    test) exit "${FAKE_TEST_RC:-0}" ;;
    build)
        out="" ver=dev
        while [ $# -gt 0 ]; do
            case "$1" in
                -o) out="$2"; shift 2 ;;
                -ldflags) ver="${2##*main.version=}"; shift 2 ;;
                *) shift ;;
            esac
        done
        pad=1100000
        [ "${FAKE_TINY_BUILD:-0}" = 1 ] && pad=0
        "$MKFAKEBIN" "$ver" "$out" "$pad" ;;
esac
EOF

# The Pi's HTTP server: /healthz for whichever build systemctl last started.
# Builds stamped 0ld* are the ones there before the test; the FAKE_NEW_*
# knobs break only the others (the build being deployed).
cat >"$STUBS/curl" <<'EOF'
#!/usr/bin/env bash
[ "${FAKE_HTTP_DOWN:-0}" = 1 ] && exit 7
url="${*: -1}" path="/${url#http://*/}"
running=$(cat "$FAKE_STATE/running")
[ "$running" = DOWN ] && exit 7
ver=$(sed -n '2s/^# FAKEBIN version=//p' "$(cat "$FAKE_STATE/running_path")")
started=$(cat "$FAKE_STATE/started")
status=ok ready=true mqtt=true sensors="${FAKE_SENSORS:-10}" detail=""
[ "${FAKE_MQTT_DOWN:-0}" = 1 ] && mqtt=false
[ "${FAKE_NOT_READY:-0}" = 1 ] && ready=false
[ "${FAKE_DEGRADED:-0}" = 1 ] && status=degraded detail="tank rig-bus-a: 1 of 5 sensors failed"
case "$ver" in
    0ld*) [ "${FAKE_OLD_NOT_READY:-0}" = 1 ] && ready=false ;;
    *)
        [ "${FAKE_NEW_MQTT_DOWN:-0}" = 1 ] && mqtt=false
        [ "${FAKE_NEW_NOT_READY:-0}" = 1 ] && ready=false
        [ "${FAKE_NEW_STARTING:-0}" = 1 ] && status=starting
        [ "${FAKE_NEW_DEGRADED:-0}" = 1 ] && status=degraded detail="tank rig-bus-b: 1 of 5 sensors failed"
        [ -n "${FAKE_NEW_SENSORS:-}" ] && sensors="$FAKE_NEW_SENSORS"
        [ -n "${FAKE_NEW_REPORTS:-}" ] && ver="$FAKE_NEW_REPORTS"
        if [ "${FAKE_CRASHLOOP:-0}" = 1 ]; then
            n=$(($(cat "$FAKE_STATE/calls" 2>/dev/null || echo 0) + 1)); echo $n >"$FAKE_STATE/calls"
            started="2026-10-30T10:00:0${n: -1}Z"
        fi ;;
esac
[ "$ready" = true ] || status=starting
[ "$mqtt" = true ] || [ "$status" != ok ] || status=degraded detail="mqtt not connected"
a=$((sensors / 2)) b=$((sensors - sensors / 2))
case "$path" in
    /healthz)
        printf '{"status":"%s","detail":"%s","version":"%s","start_time":"%s","uptime_s":5,"ready":%s,"mqtt_connected":%s,"tanks":[{"device_id":"rig-bus-a","sensors_ok":%d,"sensors_failed":0},{"device_id":"rig-bus-b","sensors_ok":%d,"sensors_failed":0}]}\n' \
            "$status" "$detail" "$ver" "$started" "$ready" "$mqtt" "$a" "$b" ;;
    *) exit 22 ;;
esac
EOF

# The broker's retained {prefix}/status: a HEARTBEAT from the running build.
cat >"$STUBS/mosquitto_sub" <<'EOF'
#!/usr/bin/env bash
echo "mosquitto_sub $*" >>"$FAKE_STATE/mosq"
[ "${FAKE_NO_BROKER:-0}" = 1 ] && exit 5
running=$(cat "$FAKE_STATE/running")
if [ "$running" = DOWN ]; then echo '{"online":false,"event":"LWT"}'; exit 0; fi
ver=$(sed -n '2s/^# FAKEBIN version=//p' "$(cat "$FAKE_STATE/running_path")")
event=HEARTBEAT
[ -n "${FAKE_HB_VERSION:-}" ] && ver="$FAKE_HB_VERSION"
[ -n "${FAKE_HB_EVENT:-}" ] && event="$FAKE_HB_EVENT"
printf '{"online":true,"event":"%s","timestamp":"2026-10-30T10:00:00Z","version":"%s","ready":true}\n' "$event" "$ver"
EOF

# ---------------------------------------------------------------- remote stubs

cat >"$RSTUBS/sudo" <<'EOF'
#!/usr/bin/env bash
as=""
while [ $# -gt 0 ]; do
    case "$1" in
        -n) shift ;;
        -u) as="$2"; shift 2 ;;
        *) break ;;
    esac
done
[ "${FAKE_SUDO_PW:-0}" = 1 ] && exit 1
if [ -n "$as" ]; then
    id "$as" >/dev/null 2>&1 || { echo "sudo: unknown user $as" >&2; exit 1; }
    FAKE_RUN_AS="$as" exec "$@"
fi
FAKE_AS_ROOT=1 exec "$@"
EOF

# systemctl, for two units: tanktemp and templog. State in
# $FAKE_STATE/{active,enabled}.<unit>; "not-found" means no such unit.
# tanktemp's unit as systemd last loaded it is $FAKE_STATE/loaded_unit
# (daemon-reload copies it from the unit link); starting it runs its
# ExecStart (/opt/tanktemp/... is DEPLOY_PI_DIR, /usr/local/bin/tanktemp is
# DEPLOY_BIN_LINK) as its User=, and fails as systemd would (203/EXEC) when
# that user cannot reach the build, or cannot read the config.
cat >"$RSTUBS/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "systemctl $*" >>"$FAKE_STATE/log"
now=0 args=()
for a in "$@"; do
    case "$a" in
        --now) now=1 ;;
        --*) ;;
        *) args+=("$a") ;;
    esac
done
cmd="${args[0]}" unit="${args[${#args[@]}-1]}"
unit="${unit%.service}"
down() { echo DOWN >"$FAKE_STATE/running"; echo "$1" >&2; exit 1; }
start() {
    [ -f "$FAKE_STATE/loaded_unit" ] || { echo "Unit tanktemp.service not found." >&2; exit 5; }
    exe=$(sed -n 's/^ExecStart=\([^ ]*\).*/\1/p' "$FAKE_STATE/loaded_unit")
    user=$(sed -n 's/^User=//p' "$FAKE_STATE/loaded_unit")
    user="${user:-root}"
    case "$exe" in
        /opt/tanktemp/*) exe="$DEPLOY_PI_DIR/${exe#/opt/tanktemp/}" ;;
        /usr/local/bin/tanktemp) exe="$DEPLOY_BIN_LINK" ;;
    esac
    p="$exe" via_home=0 n=0
    while :; do
        case "$p" in "$HOME"/*) via_home=1 ;; esac
        [ -L "$p" ] && [ $n -lt 10 ] || break
        t=$(readlink "$p") n=$((n + 1))
        case "$t" in /*) p="$t" ;; *) p="$(dirname "$p")/$t" ;; esac
    done
    target=$(basename "$p")
    if [ ! -f "$p" ] || { [ "$user" = tanktemp ] && [ $via_home = 1 ]; }; then
        down "tanktemp.service: Main process exited, code=exited, status=203/EXEC"
    fi
    "$RSTUBS/fake_can_read" "$user" "$DEPLOY_CONFIG_PATH" || down "tanktemp: open $DEPLOY_CONFIG_PATH: permission denied"
    # FAKE_RESTART_FAIL=GLOB: starting a build that matches fails.
    # shellcheck disable=SC2053 # a glob on purpose
    if [ -n "${FAKE_RESTART_FAIL:-}" ] && [[ "$target" == $FAKE_RESTART_FAIL ]]; then
        echo DOWN >"$FAKE_STATE/running"
        echo "Job for tanktemp.service failed." >&2
        exit 1
    fi
    echo "$target" >"$FAKE_STATE/running"
    echo "$p" >"$FAKE_STATE/running_path"
    echo "$user" >"$FAKE_STATE/running_user"
    echo active >"$FAKE_STATE/active.tanktemp"
    inv="inv$(wc -l <"$FAKE_STATE/log" | tr -d ' ')"
    echo "$inv" >"$FAKE_STATE/invocation"
    echo "$inv tanktemp $target starting" >>"$FAKE_STATE/journal"
    [ "${FAKE_PANIC:-0}" = 1 ] && echo "$inv panic: runtime error: index out of range" >>"$FAKE_STATE/journal"
    printf '2026-10-30T10:%02d:00Z\n' "$(wc -l <"$FAKE_STATE/log" | tr -d ' ')" >"$FAKE_STATE/started"
}
case "$cmd" in
    is-active)
        s=$(cat "$FAKE_STATE/active.$unit"); [ "$s" = not-found ] && s=inactive
        echo "$s"; [ "$s" = active ] ;;
    is-enabled)
        s=$(cat "$FAKE_STATE/enabled.$unit")
        if [ "$s" = not-found ]; then echo "Failed to get unit file state for $unit.service: No such file or directory" >&2; exit 1; fi
        echo "$s"; [ "$s" = enabled ] ;;
    enable)
        echo enabled >"$FAKE_STATE/enabled.$unit"
        if [ $now = 1 ]; then [ "$unit" = tanktemp ] && start || echo active >"$FAKE_STATE/active.$unit"; fi ;;
    disable)
        echo disabled >"$FAKE_STATE/enabled.$unit"
        [ $now = 0 ] || echo inactive >"$FAKE_STATE/active.$unit" ;;
    stop) echo inactive >"$FAKE_STATE/active.$unit" ;;
    show)
        prop="" prev=""
        for a in "$@"; do [ "$prev" = -p ] && prop="$a"; prev="$a"; done
        case "$prop" in
            InvocationID) cat "$FAKE_STATE/invocation" ;;
            MainPID) [ "$(cat "$FAKE_STATE/active.$unit")" = active ] && echo 4242 || echo 0 ;;
            NeedDaemonReload)
                if [ -f "$FAKE_STATE/loaded_unit" ] && [ -e "$DEPLOY_UNIT_LINK" ] && ! cmp -s "$FAKE_STATE/loaded_unit" "$DEPLOY_UNIT_LINK"; then
                    echo yes
                else
                    echo no
                fi ;;
        esac ;;
    status) echo "tanktemp.service - fake" ;;
    daemon-reload)
        [ "${FAKE_RELOAD_FAIL:-0}" = 1 ] && exit 1
        if [ -e "$DEPLOY_UNIT_LINK" ]; then cp "$DEPLOY_UNIT_LINK" "$FAKE_STATE/loaded_unit"; else rm -f "$FAKE_STATE/loaded_unit"; fi ;;
    restart | start) start ;;
esac
EOF

cat >"$RSTUBS/journalctl" <<'EOF'
#!/usr/bin/env bash
# Lines in $FAKE_STATE/journal are "<invocation> <message>".
[ "${FAKE_JOURNAL_BROKEN:-0}" = 1 ] && { echo "journal is corrupt" >&2; exit 1; }
want=""
for a in "$@"; do case "$a" in _SYSTEMD_INVOCATION_ID=*) want="${a#*=}" ;; esac; done
while read -r inv msg; do
    [ -z "$want" ] || [ "$inv" = "$want" ] && echo "$msg"
done <"$FAKE_STATE/journal"
true
EOF

# install: as root it could chown; here it logs that it was asked to and
# records the owner in $FAKE_STATE/owners ("<path> <user>:<group>" lines,
# the last one for a path wins; a path with none is the deploy user's).
cat >"$RSTUBS/install" <<'EOF'
#!/usr/bin/env bash
echo "install $*" >>"$FAKE_STATE/log"
args=() o="" g="" dirs=0
while [ $# -gt 0 ]; do
    case "$1" in
        -o) o="$2"; shift 2 ;;
        -g) g="$2"; shift 2 ;;
        -d) dirs=1; args+=("$1"); shift ;;
        *) args+=("$1"); shift ;;
    esac
done
PATH="${PATH//$RSTUBS:/}" install "${args[@]}" || exit
if [ -n "$o$g" ]; then
    if [ $dirs = 1 ]; then
        for a in "${args[@]}"; do case "$a" in -* | 0[0-7]*) ;; *) echo "$a ${o:-root}:${g:-root}" >>"$FAKE_STATE/owners" ;; esac; done
    else
        echo "${args[${#args[@]}-1]} ${o:-root}:${g:-root}" >>"$FAKE_STATE/owners"
    fi
fi
EOF

cat >"$RSTUBS/chown" <<'EOF'
#!/usr/bin/env bash
echo "chown $*" >>"$FAKE_STATE/log"
[ "${FAKE_AS_ROOT:-0}" = 1 ] || { echo "chown: Operation not permitted" >&2; exit 1; }
og="$1"; shift
case "$og" in *:*) ;; *) og="$og:root" ;; esac
for f; do [ -e "$f" ] || { echo "chown: $f: No such file" >&2; exit 1; }; echo "$f $og" >>"$FAKE_STATE/owners"; done
EOF

# owner_of PATH: user:group, from $FAKE_STATE/owners or else the deploy user.
cat >"$RSTUBS/owner_of" <<'EOF'
#!/usr/bin/env bash
og=$(awk -v f="$1" '$1 == f {r = $2} END {print r}' "$FAKE_STATE/owners" 2>/dev/null)
echo "${og:-$(PATH="${PATH//$RSTUBS:/}" id -un):$(PATH="${PATH//$RSTUBS:/}" id -gn)}"
EOF

# stat -c FORMAT FILE, for %U %G %a: the owner as recorded, the real mode.
cat >"$RSTUBS/stat" <<'EOF'
#!/usr/bin/env bash
real() { PATH="${PATH//$RSTUBS:/}" "$@"; }
[ "$1" = -c ] || { real stat "$@"; exit; }
fmt="$2" f="$3"
[ -e "$f" ] || { echo "stat: cannot statx '$f': No such file or directory" >&2; exit 1; }
og=$("$RSTUBS/owner_of" "$f")
mode=$(real stat -c %a "$f" 2>/dev/null || real stat -f %Lp "$f") # GNU first: GNU's -f is file-system status
out="${fmt//%U/${og%%:*}}"
out="${out//%G/${og#*:}}"
echo "${out//%a/$mode}"
EOF

# fake_can_read USER FILE: could USER read FILE on the Pi, by its recorded
# owner and its real mode?
cat >"$RSTUBS/fake_can_read" <<'EOF'
#!/usr/bin/env bash
user="$1" f="$2"
[ "$user" != root ] || exit 0
[ -e "$f" ] || exit 1
og=$(awk -v f="$f" '$1 == f {r = $2} END {print r}' "$FAKE_STATE/owners" 2>/dev/null)
[ -n "$og" ] || exit 0
mode=$(PATH="${PATH//$RSTUBS:/}" stat -c %a "$f" 2>/dev/null || PATH="${PATH//$RSTUBS:/}" stat -f %Lp "$f")
mode="${mode: -3}"
if [ "$user" = "${og%%:*}" ]; then d="${mode:0:1}"
elif [ "$user" = tanktemp ] && [ "${og#*:}" = tanktemp ]; then d="${mode:1:1}"
else d="${mode:2:1}"; fi
[ $((d & 4)) -ne 0 ]
EOF

# id: the tanktemp user exists once $FAKE_STATE/user.tanktemp does (its
# content is its group); anyone else is the real id.
cat >"$RSTUBS/id" <<'EOF'
#!/usr/bin/env bash
if [ "${*: -1}" = tanktemp ]; then
    [ -f "$FAKE_STATE/user.tanktemp" ] || { echo "id: 'tanktemp': no such user" >&2; exit 1; }
    g=$(cat "$FAKE_STATE/user.tanktemp")
    case "$1" in
        -un) echo tanktemp ;;
        -gn | -Gn) echo "$g" ;;
        -u) echo 990 ;;
        *) echo "uid=990(tanktemp) gid=990($g) groups=990($g)" ;;
    esac
    exit 0
fi
PATH="${PATH//$RSTUBS:/}" exec id "$@"
EOF

cat >"$RSTUBS/useradd" <<'EOF'
#!/usr/bin/env bash
echo "useradd $*" >>"$FAKE_STATE/log"
[ "${FAKE_AS_ROOT:-0}" = 1 ] || { echo "useradd: Permission denied." >&2; exit 1; }
[ "${FAKE_USERADD_FAIL:-0}" != 1 ] || { echo "useradd: cannot lock /etc/passwd" >&2; exit 1; }
[ ! -f "$FAKE_STATE/user.${*: -1}" ] || { echo "useradd: user '${*: -1}' already exists" >&2; exit 9; }
echo "${*: -1}" >"$FAKE_STATE/user.${*: -1}"
EOF

# ps -o user= -p PID: who the running tanktemp runs as.
cat >"$RSTUBS/ps" <<'EOF'
#!/usr/bin/env bash
cat "$FAKE_STATE/running_user"
EOF

for cmd in reboot shutdown; do
    # shellcheck disable=SC2016 # $* and $FAKE_STATE are for the stub
    printf '#!/usr/bin/env bash\necho "REBOOT %s $*" >>"$FAKE_STATE/log"\n' "$cmd" >"$RSTUBS/$cmd"
done

cat >"$RSTUBS/uname" <<'EOF'
#!/usr/bin/env bash
echo "${FAKE_ARCH:-aarch64}"
EOF

cat >"$RSTUBS/sha256sum" <<'EOF'
#!/usr/bin/env bash
if [ "${FAKE_CORRUPT:-0}" = 1 ]; then echo "0000  $1"; else shasum -a 256 "$@"; fi
EOF

chmod +x "$STUBS"/* "$RSTUBS"/*

# ---------------------------------------------------------------- fixtures

OLD1=tanktemp-20261001-100000-0ld0001
OLD2=tanktemp-20261010-100000-0ld0002
CUR=tanktemp-20261020-100000-0ld0003

# The builds already on the Pi, made once and hard-linked into each case:
# macOS checks each new executable the first time it runs (~0.2 s), and a
# link shares the check.
TPL="$WORK/tpl"
mkdir -p "$TPL"
for b in "$OLD1" "$OLD2" "$CUR"; do
    "$MKFAKEBIN" "${b##*-}" "$TPL/$b"
    "$TPL/$b" -version >/dev/null
done

config_yaml() {
    cat <<'EOF'
sweep: {interval: 30s}
mqtt:
  broker: tcp://127.0.0.1:1883
  topic_prefix: test/energy/hot_water
tanks:
  - device_id: rig-bus-a
    sensors:
      - {rom: 28-000000000001, position: a1, order: 1}
EOF
}

ME=$(id -un) MYGROUP=$(id -gn)

# fresh: a fake Pi set up and running $CUR healthily as the tanktemp user
# from /opt/tanktemp (here $CASE/opt/tanktemp, owned by the deploy user),
# with its config root:tanktemp 0640, templog off, and a clean, pushed
# checkout whose deploy/host names the Pi.
fresh() {
    rm -rf "$CASE"
    mkdir -p "$CASE"/{home,opt/tanktemp,state,bin,etc/tanktemp,boot,w1/w1_bus_master1,w1/w1_bus_master2}
    FAKE_HOME="$CASE/home" FAKE_STATE="$CASE/state"
    PI="$CASE/opt/tanktemp" OLDPI="$FAKE_HOME/tanktemp"
    CFG="$CASE/etc/tanktemp/config.yaml"
    local b n=1
    for b in "$OLD1" "$OLD2" "$CUR"; do
        ln "$TPL/$b" "$PI/$b"
        touch -t "20250${n}011200" "$PI/$b" # distinct mtimes for ls -t
        n=$((n + 1))
    done
    ln -s "$CUR" "$PI/tanktemp-bin"
    cp "$HERE/tanktemp.service" "$PI/tanktemp.service"
    ln -s "$PI/tanktemp-bin" "$CASE/bin/tanktemp"
    ln -s "$PI/tanktemp.service" "$CASE/etc/tanktemp.service"
    config_yaml >"$CFG"
    chmod 0640 "$CFG"
    echo "$CFG root:tanktemp" >"$FAKE_STATE/owners"
    echo tanktemp >"$FAKE_STATE/user.tanktemp"
    cp "$HERE/tanktemp.service" "$FAKE_STATE/loaded_unit"
    echo tanktemp >"$FAKE_STATE/running_user"
    echo "$PI/$CUR" >"$FAKE_STATE/running_path"
    printf '[all]\ndtparam=audio=on\ndtoverlay=w1-gpio,gpiopin=4\ndtoverlay=w1-gpio,gpiopin=17\n' >"$CASE/boot/config.txt"
    echo active >"$FAKE_STATE/active.tanktemp"
    echo enabled >"$FAKE_STATE/enabled.tanktemp"
    echo inactive >"$FAKE_STATE/active.templog"
    echo disabled >"$FAKE_STATE/enabled.templog"
    echo "$CUR" >"$FAKE_STATE/running"
    echo 2026-10-20T06:14:53Z >"$FAKE_STATE/started"
    : >"$FAKE_STATE/log"
    : >"$FAKE_STATE/go"
    : >"$FAKE_STATE/checks"
    : >"$FAKE_STATE/mosq"
    echo inv0 >"$FAKE_STATE/invocation"
    echo "inv0 tanktemp $CUR running" >"$FAKE_STATE/journal"

    REPO="$CASE/repo"
    git init -q --bare -b main "$CASE/origin.git"
    git init -q -b main "$REPO"
    mkdir -p "$REPO/deploy" "$REPO/cmd/tanktemp"
    cp "$HERE/deploy.sh" "$HERE/setup-pi.sh" "$HERE/tanktemp.service" "$HERE/.gitignore" "$REPO/"
    cp "$HERE/deploy/lib.sh" "$HERE/deploy/config.example.yaml" "$REPO/deploy/"
    echo 'package main' >"$REPO/cmd/tanktemp/main.go"
    echo 'module x' >"$REPO/go.mod"
    git -C "$REPO" add -A
    git -C "$REPO" -c user.name=t -c user.email=t@t commit -qm fixture
    git -C "$REPO" remote add origin "$CASE/origin.git"
    git -C "$REPO" push -q origin main
    VERSION=$(git -C "$REPO" rev-parse --short=7 HEAD)
    # Both gitignored: the checkout stays clean.
    echo "t@pi.test" >"$REPO/deploy/host"
    config_yaml >"$REPO/deploy/config.local.yaml"
}

# old_unit: the unit the first install ran under: as the deploy user, from
# /usr/local/bin/tanktemp, with ProtectHome=read-only.
old_unit() {
    sed -e "s/^User=tanktemp\$/User=$ME/" -e '/^Group=/d' -e 's/^ProtectHome=yes$/ProtectHome=read-only/' \
        -e 's#^ExecStart=/opt/tanktemp/tanktemp-bin #ExecStart=/usr/local/bin/tanktemp #' "$HERE/tanktemp.service"
}

# old_layout: the Pi as the first install left it: the builds, the link, the
# unit and .units in ~/tanktemp, /usr/local/bin/tanktemp and the systemd unit
# linked into it, tanktemp running $CUR as the deploy user, no tanktemp user,
# no /opt/tanktemp and the config root:root 0644.
old_layout() {
    fresh
    mv "$PI" "$OLDPI"
    rmdir "$CASE/opt"
    old_unit >"$OLDPI/tanktemp.service"
    mkdir "$OLDPI/.units"
    cp "$OLDPI/tanktemp.service" "$OLDPI/.units/$CUR.service"
    echo "$OLD2" >"$OLDPI/.previous"
    ln -sfn "$OLDPI/tanktemp-bin" "$CASE/bin/tanktemp"
    ln -sfn "$OLDPI/tanktemp.service" "$CASE/etc/tanktemp.service"
    chmod 0644 "$CFG"
    echo "$CFG root:root" >"$FAKE_STATE/owners"
    rm "$FAKE_STATE/user.tanktemp"
    cp "$OLDPI/tanktemp.service" "$FAKE_STATE/loaded_unit"
    echo "$ME" >"$FAKE_STATE/running_user"
    echo "$OLDPI/$CUR" >"$FAKE_STATE/running_path"
}

# bare: a Pi with 1-Wire set up and templog running, nothing of tanktemp.
bare() {
    fresh
    rm -rf "$CASE/opt" "$CASE/bin/tanktemp" "$CASE/etc/tanktemp.service" "$CASE/etc/tanktemp"
    rm -f "$FAKE_STATE/user.tanktemp" "$FAKE_STATE/loaded_unit" "$FAKE_STATE/running_user"
    : >"$FAKE_STATE/owners"
    echo DOWN >"$FAKE_STATE/running"
    echo not-found >"$FAKE_STATE/active.tanktemp"
    echo not-found >"$FAKE_STATE/enabled.tanktemp"
    echo active >"$FAKE_STATE/active.templog"
    echo enabled >"$FAKE_STATE/enabled.templog"
}

pass=0 fail=0
OUT="" RC=0 INPUT=""

# run SCRIPT [VAR=value...] -- [args...]. Stdin is $INPUT (empty: EOF, so a
# prompt without --yes is declined). Through bash, not exec: the copy is a new
# file each case, and macOS's first-run check of it costs more than the run.
run() {
    local script="$1" envs=()
    shift
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
    [ $# -gt 0 ] && shift
    OUT=$(env PATH="$STUBS:$PATH" RSTUBS="$RSTUBS" MKFAKEBIN="$MKFAKEBIN" FAKE_HOME="$FAKE_HOME" FAKE_STATE="$FAKE_STATE" \
        DEPLOY_PI_DIR="$PI" DEPLOY_BIN_LINK="$CASE/bin/tanktemp" DEPLOY_UNIT_LINK="$CASE/etc/tanktemp.service" \
        DEPLOY_CONFIG_PATH="$CASE/etc/tanktemp/config.yaml" DEPLOY_BOOT_CONFIG="$CASE/boot/config.txt" \
        DEPLOY_W1_DEVICES="$CASE/w1" DEPLOY_HTTP_HOST=pi.test DEPLOY_VERIFY_TIMEOUT=2 DEPLOY_SETTLE=0 \
        DEPLOY_POLL=0.1 DEPLOY_BROKER_TIMEOUT=1 ${envs[@]+"${envs[@]}"} \
        bash "$REPO/$script" "$@" 2>&1 <<<"$INPUT") && RC=0 || RC=$?
}
deploy() { run deploy.sh "$@"; }
setup() { run setup-pi.sh "$@"; }

# count GLOB: how many entries in the fake Pi's /opt/tanktemp match.
count() { find "$PI" -mindepth 1 -maxdepth 1 -name "$1" | wc -l | tr -d ' '; }
link() { readlink "$PI/tanktemp-bin"; }
restarts() { grep -c 'systemctl restart' "$FAKE_STATE/log" || true; }
reloads() { grep -c 'systemctl daemon-reload' "$FAKE_STATE/log" || true; }
logged() { grep -qE -- "$1" "$FAKE_STATE/log"; }
running() { cat "$FAKE_STATE/running"; }
unit_is() { cmp -s "$PI/tanktemp.service" "$1"; }
pi_link() { readlink "$1" || echo MISSING; }
# snapshot: everything on the fake Pi that a read-only run must not change.
snapshot() {
    (cd "$CASE" && find home etc bin boot w1 opt -print 2>/dev/null | sort | while read -r f; do
        if [ -L "$f" ]; then echo "$f -> $(readlink "$f")"; elif [ -f "$f" ]; then echo "$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f") $(shasum "$f")"; else echo "$f/"; fi
    done
    cat state/active.* state/enabled.* state/running state/owners
    cat state/loaded_unit state/running_user state/user.* 2>/dev/null || true)
}
change_unit() {
    echo "# changed" >>"$REPO/tanktemp.service"
    git -C "$REPO" -c user.name=t -c user.email=t@t commit -qam unit
    git -C "$REPO" push -q origin main
    VERSION=$(git -C "$REPO" rev-parse --short=7 HEAD)
}
orphan_units_none() {
    local u
    for u in "$PI/.units"/*.service; do
        [ -e "$u" ] || continue
        [ -e "$PI/$(basename "$u" .service)" ] || return 1
    done
}
nothing_set_up() {
    [ ! -e "$PI" ] && [ ! -e "$FAKE_STATE/user.tanktemp" ] && [ "$(pi_link "$CASE/bin/tanktemp")" = MISSING ] &&
        [ "$(pi_link "$CASE/etc/tanktemp.service")" = MISSING ] &&
        [ "$(cat "$FAKE_STATE/enabled.tanktemp")" = not-found ] && [ ! -e "$CASE/etc/tanktemp/config.yaml" ]
}
# owner_is PATH USER:GROUP MODE: as the stubs recorded it, and its real mode.
owner_is() { [ "$(env RSTUBS="$RSTUBS" FAKE_STATE="$FAKE_STATE" PATH="$RSTUBS:$PATH" stat -c '%U:%G %a' "$1")" = "$2 $3" ]; }
runs_as() { [ "$(cat "$FAKE_STATE/running_user")" = "$1" ]; }
runs_from_opt() { [ "$(cat "$FAKE_STATE/running_path")" = "$PI/$(running)" ]; }
runs_as_tanktemp_from_opt() { runs_as tanktemp && runs_from_opt; }
old_runs_untouched() { runs_as "$ME" && [ "$(restarts)" -eq 0 ]; }
restarted_once_as_tanktemp() { runs_as tanktemp && [ "$(restarts)" -eq 1 ]; }
builds_copied() { local b; for b in "$OLD1" "$OLD2" "$CUR"; do cmp -s "$OLDPI/$b" "$PI/$b" || return 1; done; }
config_unchanged() { config_yaml | cmp -s - "$CFG"; }
newest_build() { find "$PI" -maxdepth 1 -name 'tanktemp-2*' -print0 | xargs -0 ls -1t | head -1 | xargs basename; }
templog_on() { [ "$(cat "$FAKE_STATE/active.templog")" = active ] && [ "$(cat "$FAKE_STATE/enabled.templog")" = enabled ]; }

check() { # check NAME CONDITION...
    local name="$1"; shift
    if "$@"; then
        echo "ok   $name"; pass=$((pass + 1))
    else
        echo "FAIL $name"
        printf '%s\n' "$OUT" | sed 's/^/     | /'
        fail=$((fail + 1))
    fi
}
not() { ! "$@"; }
all_executable() { local f; for f; do [ -x "$f" ] || return 1; done; }
ends_with() { [ "${1%"$2"}" != "$1" ]; }
rc_is() { [ "$RC" -eq "$1" ]; }
says() { grep -qE -- "$1" <<<"$OUT"; }
link_is() { [ "$(link)" = "$1" ]; }
new_build_is_running() { [[ "$(link)" == tanktemp-2*-"$VERSION" ]] && [ "$(running)" = "$(link)" ]; }
rolled_back() { link_is "$CUR" && [ "$(running)" = "$CUR" ] && rc_is 1; }
untouched() { link_is "$CUR" && [ "$(restarts)" -eq 0 ] && [ "$(count 'tanktemp-2*')" -eq 3 ] && [ "$(count '.incoming-*')" -eq 0 ]; }

# ================================================================ deploy.sh

case_01() {
    fresh
    before=$(snapshot)
    deploy -- --check
    check "--check passes on a healthy Pi" rc_is 0
    check "--check names the host from deploy/host" says 'checking t@pi.test'
    check "--check reports the running build" says "running build: $CUR"
    check "--check reports the probes reading now" says 'sensors_ok 10'
    check "--check reads the broker and prefix from -check-config" says 'publishing to test/energy/hot_water on tcp://127.0.0.1:1883'
    check "--check reads the retained status from that broker and topic" grep -q -- '-h 127.0.0.1 -p 1883 -t test/energy/hot_water/status' "$FAKE_STATE/mosq"
    check "--check runs go vet ./..." grep -q '^go vet ./...' "$FAKE_STATE/go"
    check "--check runs go test -count=1 ./..." grep -q '^go test -count=1 ./...' "$FAKE_STATE/go"
    check "--check builds nothing" [ "$(grep -c '^go build' "$FAKE_STATE/go")" -eq 0 ]
    check "--check changes nothing on the Pi" [ "$(snapshot)" = "$before" ]
    check "--check restarts nothing" [ "$(restarts)" -eq 0 ]
    check "the running build checks the config as tanktemp (it is root:tanktemp 0640)" grep -q "check-config 0ld0003 $CFG as tanktemp" "$FAKE_STATE/checks"
}

case_02() {
    fresh
    deploy -- --yes
    check "deploy --yes succeeds" rc_is 0
    check "deploy switches to the new build, named by commit" new_build_is_running
    first=$(link)
    check "the build is for linux/arm64 (aarch64), static" grep -qE "^go build .*GOOS=linux GOARCH=arm64 GOARM= CGO_ENABLED=0" "$FAKE_STATE/go"
    check "the build is -trimpath and stamped -s -w -X main.version" grep -qE -- "-trimpath -ldflags -s -w -X main.version=$VERSION " "$FAKE_STATE/go"
    check "the build uses go.work (GOWORK is not off)" not grep -q 'GOWORK=off' "$FAKE_STATE/go"
    check "the new build checks the Pi's config before the switch, as tanktemp" grep -q "check-config $VERSION $CFG as tanktemp" "$FAKE_STATE/checks"
    check "the new build runs as tanktemp" runs_as tanktemp
    check "from /opt/tanktemp" runs_from_opt
    check "deploy records the previous build" [ "$(cat "$PI/.previous")" = "$CUR" ]
    check "deploy restarts once" [ "$(restarts)" -eq 1 ]
    check "deploy confirms the broker heartbeat" says 'broker holds a HEARTBEAT from'
    check "deploy leaves no staging file" [ "$(count '.incoming-*')" -eq 0 ]
    check "deploy keeps 3 builds" [ "$(count 'tanktemp-2*')" -eq 3 ]
    check "deploy keeps the previous build" [ -f "$PI/$CUR" ]
    check "the oldest build is pruned" [ ! -e "$PI/$OLD1" ]
    check "the new build's unit file is kept beside it" cmp -s "$PI/.units/$first.service" "$HERE/tanktemp.service"
    check "and the previous build's" [ -f "$PI/.units/$CUR.service" ]
    check "/usr/local/bin/tanktemp still points at the link" [ "$(pi_link "$CASE/bin/tanktemp")" = "$PI/tanktemp-bin" ]
    check "deploy leaves the checkout clean (bin/ is ignored)" [ -z "$(git -C "$REPO" status --porcelain)" ]
    check "unit file unchanged: no daemon-reload" [ "$(reloads)" -eq 0 ]

    sleep 1 # a new timestamp in the build name
    deploy -- --yes
    check "a second deploy of the same commit succeeds" rc_is 0
    check "it keeps the first deploy as previous" [ "$(cat "$PI/.previous")" = "$first" ]
    check "it still keeps 3 builds" [ "$(count 'tanktemp-2*')" -eq 3 ]
    second=$(link)

    for order in "--check --rollback" "--rollback --check"; do
        # shellcheck disable=SC2086 # split the flags on purpose
        deploy -- $order --yes
        check "$order is read-only: exits 0" rc_is 0
        check "$order is read-only: the link is unchanged" link_is "$second"
        check "$order says what a rollback would do" says "A rollback would switch .* back to $first"
    done

    deploy -- --rollback --yes
    check "--rollback succeeds" rc_is 0
    check "--rollback switches to the previous build" link_is "$first"
    check "--rollback runs it" [ "$(running)" = "$first" ]
    check "--rollback runs it as tanktemp, from /opt/tanktemp" runs_as_tanktemp_from_opt
    check "--rollback records the build it left" [ "$(cat "$PI/.previous")" = "$second" ]
}

case_03() {
    fresh
    deploy -- --rollback --yes
    check "--rollback with nothing recorded is refused" rc_is 2
    check "and says so" says 'nothing recorded'
}

case_04() {
    fresh
    for _ in 1 2 3; do
        deploy -- --yes
        sleep 1
    done
    check "three deploys later the old builds are cleaned out" [ ! -e "$PI/$CUR" ]
    check "3 builds are kept" [ "$(count 'tanktemp-2*')" -eq 3 ]
    check "with one unit file per kept build" [ "$(find "$PI/.units" -name '*.service' | wc -l | tr -d ' ')" -eq 3 ]
    check "and no orphan unit files" orphan_units_none
}

case_05() {
    fresh
    change_unit
    deploy -- --yes
    check "a changed unit file is installed" says 'installed the unit file of'
    check "and systemd reloaded" [ "$(reloads)" -eq 1 ]
    deploy -- --rollback --yes
    check "--rollback puts the old unit file back" unit_is "$HERE/tanktemp.service"
}

# ---------------------------------------------------------------- the host

case_06() {
    fresh
    rm "$REPO/deploy/host"
    deploy -- --check
    check "no host anywhere is refused" rc_is 2
    check "it says where a host comes from" says 'deploy/host'
    check "having asked nothing of any host" [ ! -s "$FAKE_STATE/ssh_hosts" ]
    deploy -- --check other@pi2.test
    check "a host argument is used" says 'checking other@pi2.test'
    check "and ssh goes there" grep -qx 'other@pi2.test' "$FAKE_STATE/ssh_hosts"
    echo "# the Pi" >"$REPO/deploy/host" && echo " t@pi3.test " >>"$REPO/deploy/host"
    deploy -- --check
    check "deploy/host may have a comment and spaces" says 'checking t@pi3.test'
    echo "t@pi.test" >"$REPO/deploy/host"
    deploy -- --check another@pi4.test
    check "an argument wins over deploy/host" says 'checking another@pi4.test'
}

# ---------------------------------------------------------------- refusals

case_07() {
    fresh
    deploy FAKE_SSH_DOWN=1 -- --yes
    check "an unreachable Pi is refused" rc_is 2
    check "it says ssh failed" says 'cannot ssh to t@pi.test'
}

case_08() {
    fresh
    deploy FAKE_SUDO_PW=1 -- --yes
    check "sudo needing a password is refused" rc_is 2
    check "and says so" says 'needs a password'
    check "having changed nothing (sudo)" untouched
}

case_09() {
    fresh
    deploy FAKE_ARCH=x86_64 -- --yes
    check "an unknown architecture is refused" rc_is 2
    check "and named" says "is 'x86_64'"
}

case_10() {
    fresh
    deploy FAKE_ARCH=armv7l -- --check
    check "armv7l is built for" rc_is 0
}

case_11() {
    fresh
    echo "// edit" >>"$REPO/cmd/tanktemp/main.go"
    deploy -- --yes
    check "uncommitted changes are refused" rc_is 2
    check "a refused deploy changes nothing" untouched
    deploy -- --check
    check "--check reports them and fails" rc_is 2
    check "with what a deploy would refuse" says 'a deploy would refuse'
    deploy -- --allow-dirty --yes
    check "--allow-dirty deploys them" rc_is 0
    check "as <commit>-dirty" ends_with "$(link)" "-$VERSION-dirty"
}

case_12() {
    fresh
    git -C "$REPO" -c user.name=t -c user.email=t@t commit -q --allow-empty -m unpushed
    VERSION=$(git -C "$REPO" rev-parse --short=7 HEAD)
    deploy -- --yes
    check "an unpushed commit is refused" rc_is 2
    check "it says to push" says 'not on any remote branch'
    check "and names the override" says '--allow-unpushed'
    check "having changed nothing (unpushed)" untouched
    deploy -- --check
    check "--check fails on it" rc_is 2
    deploy -- --allow-unpushed --yes
    check "--allow-unpushed deploys it" rc_is 0
    check "with a warning" says 'WARNING: .*not on any remote branch'
    check "the unpushed build runs" new_build_is_running
}

case_13() {
    fresh
    git -C "$REPO" remote remove origin
    deploy -- --yes
    check "no git remote at all is refused" rc_is 2
    check "and says there is no remote" says 'no git remote'
    deploy -- --check
    check "--check says so too" says 'no git remote'
    check "and fails" rc_is 2
    deploy -- --allow-unpushed --yes
    check "--allow-unpushed deploys without a remote" rc_is 0
    check "warning that there is no remote" says 'WARNING: no git remote'
}

case_14() {
    fresh
    git -C "$REPO" remote set-url origin "$CASE/nowhere.git"
    deploy -- --yes
    check "a remote that cannot be reached is refused" rc_is 2
    check "it says so (unreachable)" says 'cannot reach the remote'
}

case_15() {
    fresh
    deploy FAKE_TEST_RC=1 -- --yes
    check "failing tests are refused" rc_is 2
    check "before anything is uploaded" untouched
}

case_16() {
    fresh
    deploy FAKE_NOT_READY=1 -- --yes
    check "a running build that is not ready is refused" rc_is 2
    check "it says the running build is not healthy" says 'running build is not healthy'
    check "having changed nothing (not ready)" untouched
    deploy FAKE_NOT_READY=1 -- --check
    check "--check fails on it too" rc_is 2
    check "but keeps checking: it still runs vet and test" says '==> go vet and go test'
}

case_17() {
    fresh
    deploy FAKE_MQTT_DOWN=1 -- --yes
    check "a running build without MQTT is refused" rc_is 2
    check "and says MQTT is not connected" says 'MQTT not connected'
}

case_18() {
    fresh
    deploy FAKE_HTTP_DOWN=1 -- --yes
    check "a running build that does not answer is refused" rc_is 2
    check "having changed nothing (no answer)" untouched
}

case_19() {
    fresh
    deploy FAKE_OLD_NOT_READY=1 -- --allow-unhealthy --yes
    check "--allow-unhealthy deploys a fix over a broken build" rc_is 0
    check "the fix runs" new_build_is_running
}

case_20() {
    fresh
    deploy FAKE_DEGRADED=1 -- --yes
    check "a degraded running build is only a warning" rc_is 0
    check "which names the detail" says 'WARNING: .*degraded.*1 of 5 sensors failed'
}

case_21() {
    fresh
    deploy FAKE_REJECT_VERSION="$VERSION" -- --yes
    check "a config the new build rejects is refused (2)" rc_is 2
    check "it quotes the new build" says 'mqtt.broker: required'
    check "the staged build is removed" [ "$(count "tanktemp-2*-$VERSION")" -eq 0 ]
    check "and its unit file" [ "$(find "$PI/.units" -name "*-$VERSION.service" 2>/dev/null | wc -l | tr -d ' ')" -eq 0 ]
    check "and nothing is switched (config)" untouched
}

case_22() {
    fresh
    rm "$CASE/etc/tanktemp/config.yaml"
    deploy -- --yes
    check "a Pi without its config is refused" rc_is 2
    check "pointing at setup-pi.sh (config)" says 'setup-pi.sh'
}

case_23() {
    bare
    deploy -- --check
    check "deploy on a bare Pi is refused" rc_is 2
    check "and points at setup-pi.sh" says 'setup-pi.sh'
}

case_24() {
    fresh
    echo active >"$FAKE_STATE/active.templog"
    deploy -- --check
    check "deploy warns if templog is running too" says 'WARNING: templog is active'
}

case_25() {
    fresh
    deploy FAKE_CORRUPT=1 -- --yes
    check "a corrupted upload is refused" says 'upload arrived corrupted'
    check "and nothing is switched (corrupt)" untouched
}

case_26() {
    fresh
    deploy FAKE_SSH_DROP_AFTER_UPLOAD=once -- --yes
    check "a dropped link after the upload is refused (2, not ssh's 255)" rc_is 2
    check "nothing is switched after the drop" untouched
}

case_27() {
    fresh
    INPUT=n deploy --
    check "answering no at the prompt declines" rc_is 2
    check "a declined deploy uploads and changes nothing" untouched
}

case_28() {
    fresh
    deploy FAKE_TINY_BUILD=1 -- --yes
    check "a build too small to be real is refused" rc_is 2
    check "saying how small" says 'the build is only'
}

# ---------------------------------------------------------------- verification

case_29() {
    fresh
    deploy FAKE_NEW_REPORTS=deadbee -- --yes
    check "a build reporting the wrong version is rolled back (1)" rolled_back
    check "it says which version it saw" says 'version is deadbee, not '
}

case_30() {
    fresh
    deploy FAKE_NEW_NOT_READY=1 -- --yes
    check "a build that never gets ready is rolled back" rolled_back
    check "it says not ready" says 'FAILED VERIFICATION: .*not ready'
}

case_31() {
    fresh
    deploy FAKE_NEW_STARTING=1 -- --yes
    check "a build stuck in status starting is rolled back" rolled_back
    check "it says starting" says 'status starting'
}

case_32() {
    fresh
    deploy FAKE_NEW_MQTT_DOWN=1 -- --yes
    check "a build without MQTT is rolled back" rolled_back
    check "it says MQTT is not connected (new)" says 'FAILED VERIFICATION: .*MQTT not connected'
}

case_33() {
    fresh
    deploy FAKE_NEW_SENSORS=9 -- --yes
    check "a build reading fewer probes than before is rolled back" rolled_back
    check "it says sensors_ok dropped" says 'sensors_ok 9, lower than 10 before the deploy'
}

case_34() {
    fresh
    deploy FAKE_NEW_SENSORS=11 -- --yes
    check "a build reading more probes is fine" rc_is 0
}

case_35() {
    fresh
    deploy FAKE_NEW_DEGRADED=1 -- --yes
    check "a degraded new build is kept" rc_is 0
    check "with a warning" says 'WARNING: .*degraded'
    check "and runs" new_build_is_running
}

case_36() {
    fresh
    deploy FAKE_PANIC=1 -- --yes
    check "a panic in the journal triggers rollback" rolled_back
    check "it quotes the panic" says 'panic: runtime error'
}

case_37() {
    fresh
    deploy FAKE_JOURNAL_BROKEN=1 -- --yes
    check "a journal that cannot be read fails verification" rolled_back
}

case_38() {
    fresh
    deploy FAKE_CRASHLOOP=1 -- --yes
    check "a crash-looping build is rolled back" rolled_back
    check "it says it did not stay up" says 'did not stay up'
}

case_39() {
    fresh
    deploy FAKE_HB_VERSION=0ld0003 -- --yes
    check "a retained status from the old build triggers rollback" rolled_back
    check "it explains the broker check" says 'not a HEARTBEAT from'
}

case_40() {
    fresh
    deploy FAKE_HB_EVENT=STARTUP -- --yes
    check "a retained STARTUP (no HEARTBEAT) triggers rollback" rolled_back
}

case_41() {
    fresh
    deploy FAKE_NO_BROKER=1 -- --yes
    check "an unreachable broker skips that check, with a warning" says 'broker check will be skipped'
    check "and the deploy still succeeds" rc_is 0
}

case_42() {
    fresh
    deploy FAKE_RESTART_FAIL="*-$VERSION" -- --yes
    check "a restart that fails is rolled back" rolled_back
    check "it says the restart failed" says 'FAILED VERIFICATION: the restart failed'
    check "the failed build is .previous, for a look" ends_with "$(cat "$PI/.previous")" "-$VERSION"
}

case_43() {
    fresh
    deploy FAKE_RESTART_FAIL="*" -- --yes
    check "a rollback that does not come up needs a person (3)" rc_is 3
    check "and says so" says 'needs a person'
}

case_44() {
    fresh
    deploy FAKE_NEW_MQTT_DOWN=1 FAKE_RESTART_FAIL="$CUR" -- --yes
    check "a failed deploy whose rollback fails exits 3" rc_is 3
}

case_45() {
    fresh
    change_unit
    deploy FAKE_NEW_NOT_READY=1 -- --yes
    check "a failed deploy with a new unit file is rolled back" rc_is 1
    check "the old unit file is back" unit_is "$HERE/tanktemp.service"
}

case_46() {
    fresh
    deploy -- --bogus
    check "an unknown option is refused" rc_is 2
    deploy -- --help
    check "--help prints the usage" says 'read-only: run every pre-flight check'

    # ---------------------------------------------------------------- deploy/lib.sh

    # lib CODE: run CODE with deploy/lib.sh sourced; its output in OUT.
}
case_47() {
    lib() { OUT=$(cd "$HERE" && bash -c "set -euo pipefail; source deploy/lib.sh; $1" 2>&1) && RC=0 || RC=$?; }
    # shellcheck disable=SC2016
    lib 'goarch_for aarch64 && echo "$GOARCH [$GOARM]"'
    check "lib: aarch64 builds as arm64" [ "$OUT" = "arm64 []" ]
    lib 'goarch_for x86_64'
    check "lib: x86_64 is not built for" rc_is 1
    lib 'version_of tanktemp-20261030-124322-3c0d98c'
    check "lib: version_of reads the stamp" [ "$OUT" = "3c0d98c" ]
    # shellcheck disable=SC2016
    lib 'health_of "{\"status\":\"degraded\",\"version\":\"abc\",\"ready\":true,\"mqtt_connected\":false,\"tanks\":[{\"sensors_ok\":4},{\"sensors_ok\":5}],\"start_time\":\"t\",\"detail\":\"a|b\"}"; echo "$H_STATUS $H_VERSION $H_READY $H_MQTT $H_SENSORS $H_START $H_DETAIL"'
    check "lib: health_of reads /healthz and sums sensors_ok" [ "$OUT" = "degraded abc true false 9 t a/b" ]
    # shellcheck disable=SC2016
    lib 'health_of "not json"; echo "[$H_STATUS][$H_SENSORS]"'
    check "lib: health_of is empty on junk" [ "$OUT" = "[][]" ]
    # shellcheck disable=SC2016
    lib 'parse_check_config "/etc/tanktemp/config.yaml: ok: 1 tank, 3 sensors, publishing to test/energy/hot_water on tcp://broker.example:1883" && echo "$CFG_PREFIX $CFG_BROKER"'
    check "lib: parse_check_config reads the prefix and the broker" [ "$OUT" = "test/energy/hot_water tcp://broker.example:1883" ]
    # shellcheck disable=SC2016
    lib 'broker_addr tcp://broker.example:1884 && echo "$BROKER_HOST $BROKER_PORT"; broker_addr mqtt://b2 && echo "$BROKER_HOST $BROKER_PORT"'
    check "lib: broker_addr splits host and port, 1883 by default" [ "$OUT" = $'broker.example 1884\nb2 1883' ]
    lib 'broker_addr ssl://b:8883'
    check "lib: broker_addr does not do TLS" rc_is 1
}

# ================================================================ setup-pi.sh

case_48() {
    bare
    before=$(snapshot)
    setup -- --check
    check "setup --check on a bare Pi passes" rc_is 0
    check "it lists what it would do" says 'Setup would'
    check "including a first build" says "build $VERSION"
    check "and stopping templog" says 'stop and disable templog'
    check "with the way back" says 'sudo systemctl enable --now templog'
    check "and installing the config" says 'install .*config.local.yaml as '
    check "it validates the local config with a host build" grep -qE '^go build .*GOOS= GOARCH= .*CGO_ENABLED=0' "$FAKE_STATE/go"
    check "setup --check changes nothing" [ "$(snapshot)" = "$before" ]
    check "setup --check leaves templog running" templog_on
}

case_49() {
    bare
    sed -i.bak 's/^dtoverlay=w1-gpio.*/#&/' "$CASE/boot/config.txt"
    cp "$CASE/boot/config.txt" "$GDIR/config.txt.before"
    for args in --check --yes; do
        setup -- "$args"
        check "no dtoverlay=w1-gpio ($args) is refused" rc_is 2
        check "it prints the line to add ($args)" says 'dtoverlay=w1-gpio,gpiopin='
        check "config.txt is not edited ($args)" cmp -s "$CASE/boot/config.txt" "$GDIR/config.txt.before"
        check "and nothing reboots ($args)" not logged REBOOT
        check "and nothing else is done ($args)" nothing_set_up
    done
    check "templog is left alone (no dtoverlay)" templog_on
}

case_50() {
    bare
    rm -rf "$CASE/w1"/w1_bus_master*
    setup -- --yes
    check "dtoverlay without a w1_bus_master is refused" rc_is 2
    check "it says the bus is not there" says 'w1_bus_master'
    check "and to reboot (by hand)" says '[Rr]eboot the Pi yourself'
    check "without rebooting" not logged REBOOT
    check "having done nothing (no bus)" nothing_set_up
}

case_51() {
    bare
    setup --
    check "declining the templog prompt stops setup (2)" rc_is 2
    check "templog still runs" templog_on
    check "nothing is set up after declining" nothing_set_up
}

case_52() {
    bare
    INPUT=$'y\ny\nn\n' setup --
    check "declining the config prompt stops there" rc_is 2
    check "after templog was stopped (it was agreed)" not templog_on
    check "and the tanktemp user was made (it was agreed)" [ -f "$FAKE_STATE/user.tanktemp" ]
    check "and says what was done stays" says 're-run'
    check "no config is installed" [ ! -e "$CASE/etc/tanktemp/config.yaml" ]
}

case_53() {
    bare
    INPUT=$'y\ny\ny\ny\n' setup --
    check "answering yes to each prompt sets up" rc_is 0
    check "templog was disabled" [ "$(cat "$FAKE_STATE/enabled.templog")" = disabled ]
}

case_54() {
    bare
    setup -- --yes
    check "setup --yes on a bare Pi succeeds" rc_is 0
    check "templog is disabled and stopped" logged 'systemctl disable --now templog'
    check "templog is off" not templog_on
    check "it prints how to bring templog back" says 'sudo systemctl enable --now templog'
    check "the config is installed" cmp -s "$CASE/etc/tanktemp/config.yaml" "$REPO/deploy/config.local.yaml"
    check "as root:tanktemp 0640" logged 'install -o root -g tanktemp -m 0640'
    check "so tanktemp can read it, and only root write it" owner_is "$CFG" root:tanktemp 640
    check "it makes the tanktemp system user" logged 'useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin tanktemp'
    check "before it installs the config" [ "$(grep -n useradd "$FAKE_STATE/log" | cut -d: -f1)" -lt "$(grep -n 'install -o root -g tanktemp' "$FAKE_STATE/log" | cut -d: -f1)" ]
    check "it makes /opt/tanktemp, the deploy user's, 0755" logged "install -d -o $ME -g $MYGROUP -m 0755 $PI"
    check "/opt/tanktemp is the deploy user's, 0755" owner_is "$PI" "$ME:$MYGROUP" 755
    check "the first build is checked as tanktemp" grep -q "check-config $VERSION $CFG as tanktemp" "$FAKE_STATE/checks"
    check "tanktemp runs as tanktemp" runs_as tanktemp
    check "from /opt/tanktemp (setup)" runs_from_opt
    check "nothing is put in the home" [ ! -e "$OLDPI" ]
    check "it links a first build, named by commit" new_build_is_running
    check "the new build checked the config on the Pi" grep -q "check-config $VERSION $CASE/etc/tanktemp/config.yaml" "$FAKE_STATE/checks"
    check "it installs the unit file" unit_is "$HERE/tanktemp.service"
    check "it links /usr/local/bin" [ "$(pi_link "$CASE/bin/tanktemp")" = "$PI/tanktemp-bin" ]
    check "it links the systemd unit" [ "$(pi_link "$CASE/etc/tanktemp.service")" = "$PI/tanktemp.service" ]
    check "it reloads systemd" [ "$(reloads)" -eq 1 ]
    check "it enables and starts the service" logged 'systemctl enable --now tanktemp'
    check "it leaves no staging file" [ "$(count '.incoming-*')" -eq 0 ]
    check "it checks the service came up" says 'is running on'
    check "config.txt is never touched" grep -q '^dtoverlay=w1-gpio,gpiopin=4' "$CASE/boot/config.txt"
    before=$(snapshot)
    setup -- --yes
    check "setup again succeeds" rc_is 0
    check "and has nothing to do" says 'already set up: nothing to do'
    check "and changes nothing on the Pi" [ "$(snapshot)" = "$before" ]
    deploy -- --check
    check "deploy --check passes on a Pi setup made" rc_is 0
}

case_55() {
    fresh
    setup -- --check
    check "setup --check on a set-up Pi has nothing to do" says 'already set up: nothing to do'
    check "and passes" rc_is 0
}

case_56() {
    fresh
    sed -i.bak 's/30s/60s/' "$REPO/deploy/config.local.yaml"
    before=$(snapshot)
    setup -- --check
    check "a changed config is reported by --check" says 'install .*config.local.yaml as '
    check "with a diff" says '^ *\+sweep: \{interval: 60s\}'
    check "and not installed by --check" [ "$(snapshot)" = "$before" ]
    setup -- --yes
    check "setup installs a changed config" rc_is 0
    check "the new config is in place" cmp -s "$CASE/etc/tanktemp/config.yaml" "$REPO/deploy/config.local.yaml"
    check "the build is not touched" link_is "$CUR"
    check "the service is restarted onto it" [ "$(restarts)" -eq 1 ]
    setup -- --yes
    check "and then has nothing to do" says 'already set up: nothing to do'
}

case_57() {
    fresh
    echo "INVALID" >>"$REPO/deploy/config.local.yaml"
    before=$(snapshot)
    setup -- --yes
    check "an invalid local config is refused" rc_is 2
    check "quoting tanktemp" says 'mqtt.broker: required'
    check "and not installed" [ "$(snapshot)" = "$before" ]
}

case_58() {
    fresh
    config_yaml >"$GDIR/other.yaml"
    echo "# another" >>"$GDIR/other.yaml"
    setup -- --config "$GDIR/other.yaml" --yes
    check "--config PATH installs that file" cmp -s "$CASE/etc/tanktemp/config.yaml" "$GDIR/other.yaml"
    setup -- --config "$GDIR/nope.yaml" --check
    check "--config with a missing file is refused" rc_is 2
    check "naming it" says 'nope.yaml'
}

case_59() {
    fresh
    rm "$REPO/deploy/config.local.yaml"
    setup -- --yes
    check "no local config: the Pi's is kept" says 'keeping'
    check "nothing to do then" says 'already set up: nothing to do'
}

case_60() {
    bare
    rm "$REPO/deploy/config.local.yaml"
    setup -- --check
    check "no config on the Pi and none given is refused" rc_is 2
    check "and says so (no config)" says 'no config'
}

case_61() {
    bare
    git -C "$REPO" -c user.name=t -c user.email=t@t commit -q --allow-empty -m unpushed
    VERSION=$(git -C "$REPO" rev-parse --short=7 HEAD)
    setup -- --yes
    check "setup refuses to build an unpushed commit" rc_is 2
    check "and says why" says 'not on any remote branch'
    check "having changed nothing (setup unpushed)" nothing_set_up
    check "templog untouched (unpushed)" templog_on
    setup -- --check
    check "setup --check fails on it" rc_is 2
    setup -- --allow-unpushed --yes
    check "setup --allow-unpushed builds it" rc_is 0
    check "and runs it" new_build_is_running
}

case_62() {
    bare
    git -C "$REPO" remote remove origin
    setup -- --yes
    check "setup with no git remote is refused" says 'no git remote'
    setup -- --allow-unpushed --yes
    check "setup --allow-unpushed without a remote works" rc_is 0
}

case_63() {
    bare
    setup FAKE_SUDO_PW=1 -- --yes
    check "setup refuses without passwordless sudo" rc_is 2
    check "having changed nothing (setup sudo)" nothing_set_up
}

case_64() {
    bare
    setup FAKE_ARCH=x86_64 -- --check
    check "setup refuses an unknown architecture" rc_is 2
}

case_65() {
    bare
    rm "$REPO/deploy/host"
    setup -- --check
    check "setup with no host is refused" rc_is 2
    check "and says where a host comes from (setup)" says 'deploy/host'
}

case_66() {
    bare
    setup FAKE_NEW_MQTT_DOWN=1 -- --yes
    check "setup whose service does not come up healthy exits 1" rc_is 1
    check "and says so (setup)" says 'did not come up healthy'
}

case_67() {
    bare
    setup FAKE_RESTART_FAIL="*" -- --yes
    check "a setup step that fails exits 3" rc_is 3
    check "and says to re-run (setup)" says 're-run'
}

case_68() {
    bare
    setup FAKE_REJECT_VERSION="$VERSION" -- --yes
    check "a first build rejecting the config on the Pi stops (3)" rc_is 3
    check "and is removed" [ "$(count 'tanktemp-2*')" -eq 0 ]
    check "and nothing is linked to it" [ "$(pi_link "$PI/tanktemp-bin")" = MISSING ]
}

case_69() {
    fresh
    # Set up by hand once: a build linked, the rest not.
    rm "$CASE/bin/tanktemp" "$CASE/etc/tanktemp.service"
    echo inactive >"$FAKE_STATE/active.tanktemp"
    echo DOWN >"$FAKE_STATE/running"
    setup -- --yes
    check "setup finishes a half-set-up Pi" rc_is 0
    check "without building: the existing build is kept" link_is "$CUR"
    check "and starts it" [ "$(running)" = "$CUR" ]
    check "nothing was built" [ "$(grep -c "^go build .*GOOS=linux" "$FAKE_STATE/go")" -eq 0 ]
}

case_70() {
    bare
    mkdir -p "$CASE/bin" && ln -s /opt/other/tanktemp "$CASE/bin/tanktemp"
    setup -- --yes
    check "a /usr/local/bin entry pointing elsewhere is refused" rc_is 2
    check "it names it" says '/opt/other/tanktemp'
    check "and it is left alone" [ "$(pi_link "$CASE/bin/tanktemp")" = /opt/other/tanktemp ]
}

case_71() {
    fresh
    setup -- --bogus
    check "setup: an unknown option is refused" rc_is 2
}

# ---------------------------------------------------------------- the tanktemp user, /opt/tanktemp

case_72() {
    unit="$HERE/tanktemp.service"
    check "the unit runs as User=tanktemp" grep -qx 'User=tanktemp' "$unit"
    check "and Group=tanktemp" grep -qx 'Group=tanktemp' "$unit"
    check "not as the deploy user, nor a DynamicUser" not grep -qE '^(User=sweeney|DynamicUser=)' "$unit"
    check "from /opt/tanktemp/tanktemp-bin" grep -qx 'ExecStart=/opt/tanktemp/tanktemp-bin -config /etc/tanktemp/config.yaml' "$unit"
    check "with the home hidden (ProtectHome=yes)" grep -qx 'ProtectHome=yes' "$unit"
    for line in AmbientCapabilities=CAP_NET_BIND_SERVICE CapabilityBoundingSet=CAP_NET_BIND_SERVICE NoNewPrivileges=true \
        ProtectSystem=strict PrivateTmp=true PrivateDevices=true ProtectKernelTunables=true ProtectKernelModules=true \
        ProtectKernelLogs=true ProtectControlGroups=true 'RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX' \
        RestrictNamespaces=true LockPersonality=true MemoryDenyWriteExecute=true MemoryMax=64M; do
        check "the unit keeps $line" grep -qx "$line" "$unit"
    done
    # shellcheck disable=SC2016
    OUT=$(cd "$HERE" && env -u DEPLOY_PI_DIR bash -c 'source deploy/lib.sh; echo "$PI_DIR $LINK $SVC_USER"')
    check "lib: the builds live in /opt/tanktemp, where the unit runs them from, as tanktemp" [ "$OUT" = "/opt/tanktemp tanktemp-bin tanktemp" ]
}

case_73() {
    old_layout
    before=$(snapshot)
    setup -- --check
    check "migrate --check passes" rc_is 0
    check "it would make the tanktemp user" says 'create the tanktemp system user'
    check "it would make the config root:tanktemp 0640" says "make $CFG root:tanktemp 0640"
    check "it would make /opt/tanktemp" says "create $PI \\($ME:$MYGROUP 0755\\)"
    check "it would copy the 3 builds there" says "copy the 3 build\\(s\\) in $OLDPI to $PI"
    check "and link the running one" says "link $PI/tanktemp-bin to $CUR"
    check "it would install the new unit file" says "install the unit file as $PI/tanktemp.service"
    check "and repoint /usr/local/bin" says "link $CASE/bin/tanktemp -> $PI/tanktemp-bin \\(now $OLDPI/tanktemp-bin\\)"
    check "and the systemd unit" says "link $CASE/etc/tanktemp.service -> $PI/tanktemp.service \\(now $OLDPI/tanktemp.service\\)"
    check "and restart it as tanktemp" says "restart tanktemp as tanktemp \\(it runs as $ME now\\)"
    check "it says the old builds are left" says "$OLDPI is left as it is"
    check "migrate --check changes nothing" [ "$(snapshot)" = "$before" ]
    check "and restarts nothing (migrate --check)" [ "$(restarts)" -eq 0 ]
    check "and builds nothing (migrate --check)" [ "$(grep -c "^go build .*GOOS=linux" "$FAKE_STATE/go")" -eq 0 ]
}

case_74() {
    old_layout
    old_before=$(cd "$OLDPI" && find . -print | sort | while read -r f; do [ -L "$f" ] && echo "$f -> $(readlink "$f")" || { [ -f "$f" ] && shasum "$f"; } || echo "$f/"; done)
    setup -- --yes
    check "migrate --yes succeeds" rc_is 0
    check "the tanktemp user is made" [ -f "$FAKE_STATE/user.tanktemp" ]
    check "the config is root:tanktemp 0640 (migrated)" owner_is "$CFG" root:tanktemp 640
    check "and its content is the same" config_unchanged
    check "/opt/tanktemp is the deploy user's, 0755 (migrated)" owner_is "$PI" "$ME:$MYGROUP" 755
    check "the 3 builds are in /opt/tanktemp" [ "$(count 'tanktemp-2*')" -eq 3 ]
    check "byte for byte" builds_copied
    check "keeping their order by mtime (for pruning)" [ "$(newest_build)" = "$CUR" ]
    check "the link is to the build that ran" link_is "$CUR"
    check ".previous comes too (for --rollback)" [ "$(cat "$PI/.previous")" = "$OLD2" ]
    check "the old units do not (they run as the deploy user)" [ ! -e "$PI/.units/$CUR.service" ]
    check "the new unit file is installed" unit_is "$HERE/tanktemp.service"
    check "/usr/local/bin is repointed" [ "$(pi_link "$CASE/bin/tanktemp")" = "$PI/tanktemp-bin" ]
    check "the systemd unit is repointed" [ "$(pi_link "$CASE/etc/tanktemp.service")" = "$PI/tanktemp.service" ]
    check "systemd is reloaded once (migrate)" [ "$(reloads)" -eq 1 ]
    check "tanktemp is restarted once" [ "$(restarts)" -eq 1 ]
    check "and runs as tanktemp (migrated)" runs_as tanktemp
    check "from /opt/tanktemp (migrated)" runs_from_opt
    check "the same build" [ "$(running)" = "$CUR" ]
    check "the build was checked as tanktemp before the restart" grep -q "check-config 0ld0003 $CFG as tanktemp" "$FAKE_STATE/checks"
    check "it says it is healthy" says 'is running on'
    check "nothing is built (migrate)" [ "$(grep -c "^go build .*GOOS=linux" "$FAKE_STATE/go")" -eq 0 ]
    old_after=$(cd "$OLDPI" && find . -print | sort | while read -r f; do [ -L "$f" ] && echo "$f -> $(readlink "$f")" || { [ -f "$f" ] && shasum "$f"; } || echo "$f/"; done)
    check "the old ~/tanktemp is left untouched" [ "$old_after" = "$old_before" ]
    check "and it says how to remove it" says "rm -rf $OLDPI"

    before=$(snapshot)
    setup -- --yes
    check "setup after the migration succeeds" rc_is 0
    check "and has nothing to do (migrated)" says 'already set up: nothing to do'
    check "and changes nothing (migrated)" [ "$(snapshot)" = "$before" ]
    check "no second restart" [ "$(restarts)" -eq 1 ]
    check "it still mentions the old builds" says "$OLDPI is left as it is"

    deploy -- --yes
    check "deploy after the migration succeeds" rc_is 0
    check "and the new build runs" new_build_is_running
    check "as tanktemp from /opt (deployed)" runs_as_tanktemp_from_opt
    check "the build it left gets the unit it ran under" cmp -s "$PI/.units/$CUR.service" "$HERE/tanktemp.service"
    check "deploy prunes in /opt/tanktemp: 3 builds kept" [ "$(count 'tanktemp-2*')" -eq 3 ]
    check "the oldest of them pruned" [ ! -e "$PI/$OLD1" ]
    check "and never the home's" [ -e "$OLDPI/$OLD1" ]
    deploy -- --rollback --yes
    check "--rollback after the migration succeeds" rc_is 0
    check "back to the migrated build" link_is "$CUR"
    check "running it" [ "$(running)" = "$CUR" ]
    check "as tanktemp, from /opt (rolled back)" runs_as_tanktemp_from_opt
}

case_75() {
    old_layout
    before=$(snapshot)
    deploy -- --yes
    check "deploy on the old layout is refused" rc_is 2
    check "pointing at setup-pi.sh to move the builds" says "setup-pi.sh.*$PI"
    check "having changed nothing (old layout)" [ "$(snapshot)" = "$before" ]
}

case_76() {
    old_layout
    setup FAKE_RELOAD_FAIL=1 -- --yes
    check "a migration whose daemon-reload fails stops (3)" rc_is 3
    check "the old process still runs, untouched" old_runs_untouched
    setup -- --check
    check "--check after it says what is left: reload" says 'reload systemd'
    check "and restart as tanktemp (interrupted)" says "restart tanktemp as tanktemp"
    setup -- --yes
    check "re-running carries on" rc_is 0
    check "and reloads" [ "$(grep -c 'systemctl daemon-reload' "$FAKE_STATE/log")" -ge 2 ]
    check "and runs as tanktemp (after the interruption)" runs_as_tanktemp_from_opt
    setup -- --yes
    check "and then has nothing to do (interrupted)" says 'already set up: nothing to do'
}

case_77() {
    old_layout
    # Stopped after the builds were copied, before the link: a partial copy
    # is left under its staging name, a whole one under its own.
    mkdir -p "$PI"
    cp -p "$OLDPI/$OLD1" "$PI/$OLD1"
    head -c 10 "$OLDPI/$CUR" >"$PI/.incoming-$CUR"
    setup -- --yes
    check "a migration stopped part way carries on" rc_is 0
    check "copying what was missing" builds_copied
    check "leaving no staged copy" [ "$(count '.incoming-*')" -eq 0 ]
    check "running as tanktemp (part way)" runs_as tanktemp
}

case_78() {
    old_layout
    INPUT=$'n\n' setup --
    check "declining the user prompt stops (2)" rc_is 2
    check "no user is made" [ ! -f "$FAKE_STATE/user.tanktemp" ]
    check "and the old service runs on" old_runs_untouched
}

case_79() {
    old_layout
    INPUT=$'y\nn\n' setup --
    check "declining the migration stops (2)" rc_is 2
    check "the old service is not restarted" old_runs_untouched
    check "and nothing is moved" [ ! -e "$PI" ]
    check "the links are as they were" [ "$(pi_link "$CASE/bin/tanktemp")" = "$OLDPI/tanktemp-bin" ]
    check "the config is as it was: the old service can still read it" owner_is "$CFG" root:root 644
    check "one prompt covers the config and the move" says 'Make the config readable by tanktemp, move tanktemp'
    INPUT=$'y\n' setup --
    check "agreeing to it migrates" rc_is 0
    check "as tanktemp (one prompt)" runs_as_tanktemp_from_opt
}

case_80() {
    fresh
    chmod 0600 "$CFG"
    echo "$CFG root:root" >>"$FAKE_STATE/owners"
    before=$(snapshot)
    setup -- --check
    check "a config tanktemp cannot read is reported" says "make $CFG root:tanktemp 0640 \\(it is root:root 600\\)"
    check "and not changed by --check" [ "$(snapshot)" = "$before" ]
    setup -- --yes
    check "setup fixes its owner and mode" owner_is "$CFG" root:tanktemp 640
    check "with chown and chmod, not a copy" logged "chown root:tanktemp $CFG"
    check "without a restart (it has read its config already)" [ "$(restarts)" -eq 0 ]
    check "and exits 0 (config perms)" rc_is 0
}

case_81() {
    fresh
    pw="$CASE/etc/tanktemp/mqtt-password"
    echo secret >"$pw" && chmod 0600 "$pw"
    echo "$pw root:root" >>"$FAKE_STATE/owners"
    for f in "$CFG" "$REPO/deploy/config.local.yaml"; do
        sed -i.bak "s#^  topic_prefix:#  password_file: $pw\\n  topic_prefix:#" "$f" && rm -f "$f.bak"
    done
    setup -- --check
    check "a password_file tanktemp cannot read is reported" says "make $pw root:tanktemp 0640"
    setup -- --yes
    check "and fixed" owner_is "$pw" root:tanktemp 640
    check "its content kept" [ "$(cat "$pw")" = secret ]
    setup -- --yes
    check "then nothing to do (password_file)" says 'already set up: nothing to do'
}

case_82() {
    fresh
    echo "$PI root:root" >>"$FAKE_STATE/owners"
    setup -- --check
    check "a /opt/tanktemp the deploy user does not own is reported" says "make $PI $ME:$MYGROUP 0755 \\(it is root:root 755\\)"
    setup -- --yes
    check "and fixed (/opt)" owner_is "$PI" "$ME:$MYGROUP" 755
    check "without a restart" [ "$(restarts)" -eq 0 ]
}

case_83() {
    fresh
    echo users >"$FAKE_STATE/user.tanktemp"
    setup -- --check
    check "a tanktemp user without its own group is refused" rc_is 2
    check "naming its group" says "group is users"
}

case_84() {
    bare
    setup FAKE_USERADD_FAIL=1 -- --yes
    check "a useradd that fails stops (3)" rc_is 3
    check "before the config is installed" [ ! -e "$CFG" ]
}

case_85() {
    fresh
    echo "$ME" >"$FAKE_STATE/running_user"
    setup -- --check
    check "a tanktemp running as someone else would be restarted" says "restart tanktemp as tanktemp \\(it runs as $ME now\\)"
    setup -- --yes
    check "and is (as tanktemp)" restarted_once_as_tanktemp
}

case_99() {
    check "deploy.sh, setup-pi.sh and deploy_test.sh are executable" all_executable "$HERE/deploy.sh" "$HERE/setup-pi.sh" "$HERE/deploy_test.sh"
}

# ================================================================ runner

# Each case_NN starts from fresh or bare in its own directory, so they run in
# parallel: JOBS workers take every JOBS-th case, in order. Output is printed
# in case order.
JOBS="${DEPLOY_TEST_JOBS:-8}"
CASES=()
while read -r _ _ name; do
    case "$name" in case_*) CASES+=("$name") ;; esac
done < <(declare -F)
worker() {
    local i c
    for ((i = $1; i < ${#CASES[@]}; i += JOBS)); do
        c="${CASES[$i]}"
        # shellcheck disable=SC2030 # each case counts in its own subshell
        (
            GDIR="$WORK/$c" CASE="$WORK/$c/case" pass=0 fail=0
            mkdir -p "$GDIR"
            "$c"
            echo "COUNTS $pass $fail"
        ) >"$WORK/$c.out" 2>&1 || echo "COUNTS 0 1 ($c crashed)" >>"$WORK/$c.out"
    done
}
for ((w = 0; w < JOBS; w++)); do worker "$w" & done
wait
for c in "${CASES[@]}"; do
    # shellcheck disable=SC2031 # the sums are of the subshells' counts
    while read -r line; do
        case "$line" in
            "COUNTS "*) read -r _ p f _ <<<"$line"; pass=$((pass + p)) fail=$((fail + f)) ;;
            *) printf '%s\n' "$line" ;;
        esac
    done <"$WORK/$c.out"
    grep -q '^COUNTS ' "$WORK/$c.out" || { echo "FAIL $c did not finish"; fail=$((fail + 1)); }
done

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
