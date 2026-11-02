# shellcheck shell=bash
# shellcheck disable=SC2034 # the scripts that source this use what it sets
#
# What deploy.sh and setup-pi.sh share, copied from boiler-sensor's and made
# tanktemp's. They source it from the repo root once they have parsed their
# arguments (REMOTE, CHECK, YES, ALLOW_*), then call need_remote. Not run on
# its own. Tests: ./deploy_test.sh, through both scripts and directly.

SERVICE=tanktemp
SVC_USER=tanktemp # the system user the service runs as (setup-pi.sh makes it)
# The builds, the link to the running one, the unit file and .previous. Owned
# by the deploy user (so a deploy uploads without sudo), mode 0755 (so
# SVC_USER can run what is there). tanktemp.service runs PI_DIR/LINK.
PI_DIR="${DEPLOY_PI_DIR:-/opt/tanktemp}"
OLD_PI_DIR=tanktemp # under the deploy user's home: the first install's layout, which setup-pi.sh moves
LINK=tanktemp-bin   # PI_DIR/<LINK> -> the running build
UNITS=.units        # PI_DIR/.units/<build>.service: each build's unit file
BIN_LINK="${DEPLOY_BIN_LINK:-/usr/local/bin/tanktemp}"
UNIT_LINK="${DEPLOY_UNIT_LINK:-/etc/systemd/system/tanktemp.service}"
CONFIG_PATH="${DEPLOY_CONFIG_PATH:-/etc/tanktemp/config.yaml}"
CONFIG_STAGE=".$SERVICE-config.yaml.incoming" # in the deploy user's home (0700): a new config on its way
# The deploy user's sudo rules: setup-pi.sh installs them (with full sudo,
# once), and then deploy.sh and config changes need no password.
SUDOERS="${DEPLOY_SUDOERS:-/etc/sudoers.d/tanktemp}"
# Where sudo looks up a command given by name (Debian's secure_path). The
# rules name each command by the full path found here, on the Pi.
SUDO_PATH="${DEPLOY_SUDO_PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}"
SUDO_CMDS="systemctl journalctl cat install cmp" # the commands sudo_rules names
BOOT_CONFIG="${DEPLOY_BOOT_CONFIG:-/boot/firmware/config.txt}"
W1_DEVICES="${DEPLOY_W1_DEVICES:-/sys/bus/w1/devices}"
VERIFY_TIMEOUT="${DEPLOY_VERIFY_TIMEOUT:-60}"
BROKER_TIMEOUT="${DEPLOY_BROKER_TIMEOUT:-20}"
POLL="${DEPLOY_POLL:-1}"
HOST_FILE=deploy/host # gitignored: one line, user@host
REMOTE="${REMOTE:-}"
CHECK="${CHECK:-0}" YES="${YES:-0}" ALLOW_DIRTY="${ALLOW_DIRTY:-0}" ALLOW_UNPUSHED="${ALLOW_UNPUSHED:-0}"
ACTION="${ACTION:-deploy}" # in "a deploy would refuse"
CHANGED=0                  # set once something on the Pi has changed

# A stalled transfer hangs rather than errors; ServerAlive turns a dead link
# into an exit code. BatchMode stops ssh waiting for a password nobody types.
SSHOPTS=(-o ConnectTimeout=15 -o ServerAliveInterval=10 -o ServerAliveCountMax=3 -o BatchMode=yes)

say() { printf '==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    WARNING: %s\n' "$*" >&2; }
refuse() {
    printf '\nREFUSED: %s\n' "$*" >&2
    if [ "$CHANGED" -eq 1 ]; then
        printf 'The steps already done on %s stay done; re-run to carry on from here.\n' "$REMOTE" >&2
    elif [ -n "$REMOTE" ]; then
        printf 'Nothing on %s was changed.\n' "$REMOTE" >&2
    else
        printf 'Nothing was changed.\n' >&2
    fi
    exit 2
}
# -n: no remote command here reads stdin, and without it ssh would swallow
# what the user types at the confirmation prompt.
r() { ssh -n "${SSHOPTS[@]}" "$REMOTE" "$@"; }

# need_remote: the Pi is the first non-option argument (REMOTE), else the one
# line of deploy/host, else refuse. The repo names no host of its own.
need_remote() {
    if [ -z "$REMOTE" ] && [ -f "$HOST_FILE" ]; then
        REMOTE=$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$HOST_FILE" | grep -m1 . || true)
    fi
    [ -n "$REMOTE" ] || refuse "no host. Give one (./$(basename "$0") user@host), or put user@host on one line in $HOST_FILE (gitignored)"
    HTTP_HOST="${DEPLOY_HTTP_HOST:-${REMOTE#*@}}"
}

# sudo_rules HOME: everything deploy.sh and setup-pi.sh run with sudo once the
# Pi is set up, one per line as RUNAS|COMMAND|RULE: the command line as the
# scripts run it (the command by name, or a full path), then the same as
# sudoers has it, if it needs a wildcard. sudoers matches the whole command
# line, so the scripts must run exactly these, arguments in this order. HOME
# is the deploy user's home ('$HOME' for the Pi's shell to fill in).
sudo_rules() {
    local h="$1"
    cat <<EOF
root|systemctl restart $SERVICE|
root|systemctl daemon-reload|
root|journalctl -u $SERVICE -n 30 --no-pager -o cat|
root|journalctl -u $SERVICE --since @0 --no-pager -o cat|journalctl -u $SERVICE --since * --no-pager -o cat
root|journalctl _SYSTEMD_INVOCATION_ID=0 --no-pager -o cat|journalctl _SYSTEMD_INVOCATION_ID\\=* --no-pager -o cat
$SVC_USER|$PI_DIR/$LINK -check-config -config $CONFIG_PATH|$PI_DIR/$SERVICE-* -check-config -config $CONFIG_PATH
root|cat $CONFIG_PATH|
root|install -o root -g $SVC_USER -m 0640 $h/$CONFIG_STAGE $CONFIG_PATH|
root|cmp -s $h/$CONFIG_STAGE $CONFIG_PATH|
EOF
}

# sudo_probe: shell for the Pi that prints one line: "full" if sudo needs no
# password for anything; else "scoped" if it needs none for each of
# sudo_rules; else "none RUNAS CMD", the first that needs one. sudo -l says a
# command is allowed even where the rule that allows it wants a password, so
# one rule is run for real first (read-only), and a complaint from sudo
# itself, rather than from journalctl, means the rules are not in force.
sudo_probe() {
    local runas cmd _ u
    printf 'if sudo -n true 2>/dev/null; then echo full\n'
    printf "elif sudo -n journalctl -u %s -n 30 --no-pager -o cat 2>&1 >/dev/null | grep -q '^sudo:'; then echo 'none root journalctl -u %s -n 30 --no-pager -o cat'\n" "$SERVICE" "$SERVICE"
    # shellcheck disable=SC2016 # $HOME is for the Pi's shell
    while IFS='|' read -r runas cmd _; do
        u=""
        [ "$runas" = root ] || u="-u $runas "
        printf 'elif ! sudo -n %s-l %s >/dev/null 2>&1; then echo "none %s %s"\n' "$u" "$cmd" "$runas" "$cmd"
    done < <(sudo_rules '$HOME')
    printf 'else echo scoped; fi\n'
}

# need_sudo ANSWER: from sudo_probe's ANSWER, SUDO_MODE full or scoped; or
# refuse, naming the command sudo wants a password for.
need_sudo() {
    case "$1" in
        full) SUDO_MODE=full ;;
        scoped)
            SUDO_MODE=scoped
            info "sudo: the scoped rules in $SUDOERS, no password (no full sudo)" ;;
        "none "*)
            local runas="${1#none }" cmd
            cmd="${runas#* }" runas="${runas%% *}"
            refuse "sudo on $REMOTE needs a password for '$cmd'$([ "$runas" = root ] || echo " as $runas"), and this script cannot type it. To fix it, run ./setup-pi.sh once while sudo on $REMOTE needs no password: it installs $SUDOERS, after which ./deploy.sh and config changes need none" ;;
        *) refuse "cannot tell what sudo allows on $REMOTE (it answered '$1')" ;;
    esac
}

# sudoers_fragment USER HOME NAME=PATH...: the sudoers fragment for the
# deploy user USER, with each command of sudo_rules named by its full PATH.
sudoers_fragment() {
    local user="$1" home="$2" runas cmd rule name pair path
    shift 2
    echo "# Managed by $SERVICE's setup-pi.sh: do not edit, the next run replaces it."
    echo "# Only what deploy.sh and setup-pi.sh run with sudo once the Pi is set up."
    while IFS='|' read -r runas cmd rule; do
        rule="${rule:-$cmd}" name="${rule%% *}"
        case "$name" in
            /*) ;;
            *)
                path=""
                for pair; do [ "${pair%%=*}" != "$name" ] || path="${pair#*=}"; done
                [ -n "$path" ] || return 1
                rule="$path${rule#"$name"}" ;;
        esac
        echo "$user ALL=($runas) NOPASSWD: $rule"
    done < <(sudo_rules "$home")
}

# would_refuse WHY: under --check, note what would be refused and carry on
# checking; --check fails at the end.
PROBLEMS=()
would_refuse() { warn "$1: a $ACTION would refuse"; PROBLEMS+=("$1"); }

# jget KEY.PATH: read a field from the JSON on stdin; empty if absent.
jget() {
    python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError:
    d = None
for k in sys.argv[1].split("."):
    d = d.get(k) if isinstance(d, dict) else None
print("" if d is None else str(d).lower() if isinstance(d, bool) else d)' "$1"
}

confirm() {
    [ "$YES" -eq 1 ] && return 0
    local reply
    printf '\n%s [y/N] ' "$1"
    read -r reply || reply=""
    case "$reply" in y | Y | yes) return 0 ;; esac
    refuse "declined"
}

# require_tools: refuse, by name, if a local command the scripts use is missing.
require_tools() {
    local cmd
    for cmd in git go ssh rsync curl python3 shasum; do
        command -v "$cmd" >/dev/null || refuse "$cmd not found"
    done
}

# goarch_for ARCH: set GOARCH and GOARM for a Pi's `uname -m`; fails for
# anything this does not build for.
goarch_for() {
    case "$1" in
        armv6l) GOARCH=arm GOARM=6 ;;
        armv7l) GOARCH=arm GOARM=7 ;;
        aarch64 | arm64) GOARCH=arm64 GOARM="" ;;
        *) return 1 ;;
    esac
}

# commit_dirty: true if uncommitted changes would go into a build.
commit_dirty() {
    ! git diff --quiet HEAD -- || [ -n "$(git ls-files --others --exclude-standard -- cmd internal go.mod go.sum)" ]
}

# commit_pushed: 0 if HEAD is on a branch of a remote as it is now, 1 if not,
# 2 if a remote cannot be reached, 3 if a remote branch has commits this
# checkout lacks (so it cannot tell: fetch first), 4 if there is no remote.
# The build names its commit in /healthz, and rewriting a commit's date
# changes its hash: deploying only what is pushed means the dates are
# settled. It asks the remote with ls-remote rather than trusting origin/*,
# and rather than fetching, which would change the checkout under --check.
commit_pushed() {
    local head remote heads sha unknown=0
    [ -n "$(git remote)" ] || return 4
    head=$(git rev-parse HEAD)
    for remote in $(git remote); do
        heads=$(git ls-remote --heads "$remote" 2>/dev/null) || return 2
        while read -r sha _; do
            [ -n "$sha" ] || continue
            [ "$sha" = "$head" ] && return 0
            if git cat-file -e "$sha^{commit}" 2>/dev/null; then
                git merge-base --is-ancestor "$head" "$sha" && return 0
            else
                unknown=1
            fi
        done <<<"$heads"
    done
    [ $unknown -eq 0 ] || return 3
    return 1
}

# check_commit: set VERSION from HEAD and check it is clean and pushed: a
# refusal, or under --check a problem noted, or with --allow-dirty /
# --allow-unpushed a warning.
check_commit() {
    local pushed=0 why=""
    VERSION=$(git rev-parse --short=7 HEAD)
    info "$(git rev-parse --abbrev-ref HEAD) at $VERSION: $(git log -1 --format='%s (%ad)' --date=iso)"
    if commit_dirty; then
        if [ "$ALLOW_DIRTY" -eq 1 ]; then
            VERSION="$VERSION-dirty"
            warn "uncommitted changes: building as $VERSION"
        elif [ "$CHECK" -eq 1 ]; then
            would_refuse "uncommitted changes (commit first$([ "$ACTION" = deploy ] && echo ', or --allow-dirty'))"
        else
            refuse "uncommitted changes. The version is the commit, so commit first$([ "$ACTION" = deploy ] && echo ' (or --allow-dirty)')"
        fi
    fi
    commit_pushed || pushed=$?
    case $pushed in
        0) return 0 ;;
        1) why="${VERSION%-dirty} is not on any remote branch. Settle its date and push it first" ;;
        2) why="cannot reach the remote, so cannot tell that ${VERSION%-dirty} is pushed" ;;
        3) why="the remote has commits this checkout lacks (git fetch, then again), so cannot tell that ${VERSION%-dirty} is pushed" ;;
        *) why="no git remote, so ${VERSION%-dirty} cannot be pushed. Settle its date, add a remote and push it first" ;;
    esac
    if [ "$ALLOW_UNPUSHED" -eq 1 ]; then
        warn "$why; going ahead (--allow-unpushed)"
    elif [ "$CHECK" -eq 1 ]; then
        would_refuse "$why (or --allow-unpushed)"
    else
        refuse "$why (or --allow-unpushed)"
    fi
}

vet_and_test() {
    say "go vet and go test"
    go vet ./... || refuse "go vet failed"
    go test -count=1 ./... >/dev/null || refuse "go test failed (run it to see why)"
    info "pass"
}

# build_for VERSION ARCH: build for ARCH (as goarch_for has set GOARCH and
# GOARM) into LOCAL_BIN, with VERSION stamped in, and refuse a build too small
# to be real. It builds through the repo's go.work, if there is one.
build_for() {
    local size
    say "building $1"
    mkdir -p bin
    LOCAL_BIN="bin/$SERVICE-linux-$2"
    GOOS=linux GOARCH=$GOARCH GOARM=$GOARM CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=$1" -o "$LOCAL_BIN" ./cmd/$SERVICE || refuse "go build failed"
    size=$(wc -c <"$LOCAL_BIN" | tr -d ' ')
    [ "$size" -gt 1000000 ] || refuse "the build is only $size bytes"
    info "$LOCAL_BIN, $size bytes"
}

# build_host: build tanktemp for this machine into HOST_BIN, to run
# -check-config here.
build_host() {
    mkdir -p bin
    HOST_BIN="bin/$SERVICE-host"
    CGO_ENABLED=0 go build -trimpath -o "$HOST_BIN" ./cmd/$SERVICE || refuse "go build (for this machine) failed"
}

# upload_build NAME FAIL: put LOCAL_BIN on the Pi as PI_DIR/NAME, through
# a staging name so a partial upload is never mistaken for a build, with
# retries and a sha256 check. If a step fails, it removes the staged file if
# it can and calls FAIL with the step, which must not return.
upload_build() {
    local name="$1" fail="$2" stage=".incoming-$1" attempt local_sum remote_sum
    say "uploading $name"
    for attempt in 1 2 3; do
        rsync -z --partial --timeout=45 -e "ssh ${SSHOPTS[*]}" "$LOCAL_BIN" "$REMOTE:$PI_DIR/$stage" && break
        warn "upload attempt $attempt failed"
        if [ "$attempt" -eq 3 ]; then
            r "rm -f $PI_DIR/$stage" 2>/dev/null || true
            "$fail" "the upload (three attempts)"
        fi
        sleep 2
    done
    local_sum=$(shasum -a 256 "$LOCAL_BIN" | cut -d' ' -f1)
    remote_sum=$(r "sha256sum $PI_DIR/$stage" | cut -d' ' -f1) || "$fail" "checking its sha256"
    if [ "$local_sum" != "$remote_sum" ]; then
        r "rm -f $PI_DIR/$stage" 2>/dev/null || true
        "$fail" "the sha256 check (the upload arrived corrupted: $remote_sum, expected $local_sum)"
    fi
    r "chmod 0755 $PI_DIR/$stage && mv $PI_DIR/$stage $PI_DIR/$name" || "$fail" "moving it into place"
    info "sha256 verified"
}

# pi_check_config NAME: run build NAME on the Pi against the installed
# config, as SVC_USER (the config is root:SVC_USER 0640, and this also shows
# that SVC_USER can run the build); its answer (tanktemp prints it on
# stderr) in CHECK_OUT. 0 if the config is valid, 1 if not, 255 if ssh failed.
pi_check_config() {
    local rc=0
    CHECK_OUT=$(r "sudo -n -u $SVC_USER $PI_DIR/$1 -check-config -config $CONFIG_PATH 2>&1") || rc=$?
    return $rc
}

# parse_check_config OUTPUT: CFG_PREFIX and CFG_BROKER from -check-config's
# "<path>: ok: ..., publishing to <prefix> on <broker>".
parse_check_config() {
    local line
    line=$(sed -n 's/.* publishing to \([^ ]*\) on \([^ ]*\)$/\1 \2/p' <<<"$1" | tail -1)
    [ -n "$line" ] || return 1
    CFG_PREFIX="${line% *}"
    CFG_BROKER="${line#* }"
}

# broker_addr URL: BROKER_HOST and BROKER_PORT from tcp://host[:port] (or
# mqtt://); fails for anything mosquitto_sub would need more for (TLS).
broker_addr() {
    local rest
    case "$1" in
        tcp://* | mqtt://*) rest="${1#*://}" ;;
        *) return 1 ;;
    esac
    rest="${rest%%/*}"
    BROKER_HOST="${rest%%:*}"
    BROKER_PORT="${rest##*:}"
    [ "$BROKER_PORT" != "$rest" ] || BROKER_PORT=1883
    [ -n "$BROKER_HOST" ]
}

# retained_status: the broker's retained {prefix}/status, or nothing.
retained_status() {
    [ -n "${BROKER_HOST:-}" ] || return 1
    mosquitto_sub -h "$BROKER_HOST" -p "$BROKER_PORT" -t "$CFG_PREFIX/status" -C 1 -W 5 2>/dev/null
}

http() { curl -sS -f -m 5 "http://$HTTP_HOST$1" 2>/dev/null; }

# version_of NAME: the commit a build file was stamped with, from its name
# (tanktemp-YYYYMMDD-HHMMSS-<version>).
version_of() {
    local rest="${1#"$SERVICE"-}"
    rest="${rest#????????-??????}"
    printf '%s' "${rest#-}"
}

# health_of BODY: from a /healthz answer, H_STATUS H_VERSION H_READY H_MQTT,
# H_SENSORS (sensors_ok summed over tanks[], empty if there are none),
# H_START and H_DETAIL. All empty for junk.
health_of() {
    local line
    line=$(python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError:
    d = None
if not isinstance(d, dict):
    d = {}
def s(v):
    return "" if v is None else str(v).lower() if isinstance(v, bool) else str(v)
t = d.get("tanks")
n = ""
if isinstance(t, list):
    n = str(sum(x.get("sensors_ok") or 0 for x in t if isinstance(x, dict)))
f = [d.get("status"), d.get("version"), d.get("ready"), d.get("mqtt_connected"), n, d.get("start_time"), d.get("detail")]
print("|".join(s(v).replace("|", "/").replace("\n", " ") for v in f))' <<<"$1")
    IFS='|' read -r H_STATUS H_VERSION H_READY H_MQTT H_SENSORS H_START H_DETAIL <<<"$line"
}

# unhealthy_reason [VERSION] [MIN_SENSORS]: after health_of, why that answer
# is not healthy, or nothing if it is. Healthy: ready, MQTT connected, status
# ok or degraded (a degraded one is the caller's warning), and if given, the
# version and at least MIN_SENSORS probes reading.
unhealthy_reason() {
    local want="${1:-}" min="${2:-}"
    if [ -n "$want" ] && [ "$H_VERSION" != "$want" ]; then
        echo "version is ${H_VERSION:-missing}, not $want"
    elif [ "$H_READY" != true ]; then
        echo "not ready (status ${H_STATUS:-missing})"
    elif [ "$H_MQTT" != true ]; then
        echo "MQTT not connected"
    elif [ "$H_STATUS" != ok ] && [ "$H_STATUS" != degraded ]; then
        echo "status ${H_STATUS:-missing}"
    elif [ -n "$min" ] && [ "${H_SENSORS:-0}" -lt "$min" ]; then
        echo "sensors_ok ${H_SENSORS:-0}, lower than $min before the deploy"
    fi
}

# wait_healthy [VERSION] [MIN_SENSORS]: wait for /healthz to be healthy as
# unhealthy_reason has it. The last answer is left in LAST_BODY (and
# health_of's fields), and why it was not healthy in LAST_REASON.
wait_healthy() {
    local deadline=$((SECONDS + VERIFY_TIMEOUT)) body
    LAST_BODY="" LAST_REASON="no answer from http://$HTTP_HOST/healthz"
    while :; do
        if body=$(http /healthz); then
            LAST_BODY="$body"
            health_of "$body"
            LAST_REASON=$(unhealthy_reason "$@")
            [ -n "$LAST_REASON" ] || return 0
        fi
        [ $SECONDS -lt $deadline ] || return 1
        sleep "$POLL"
    done
}
