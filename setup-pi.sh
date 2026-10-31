#!/usr/bin/env bash
#
# Set up a Pi to run tanktemp, so that ./deploy.sh can take over.
#
#   ./setup-pi.sh --check        read-only: say what setup would do, change nothing
#   ./setup-pi.sh                set up (says what it will do and asks before each change)
#   ./setup-pi.sh --yes          set up without asking
#   ./setup-pi.sh --config PATH  the config to install (default deploy/config.local.yaml)
#   ./setup-pi.sh --allow-unpushed  build the first install from a commit that is
#                                not pushed (or with no git remote): a warning
#   ./setup-pi.sh [opts] user@host  the Pi; without it, the one line of
#                                deploy/host (gitignored). The repo names no host.
#
# Each step runs only if it is still needed, so it is safe to re-run: on a Pi
# that is already set up it changes nothing and says so, and after a failure
# it carries on from where it stopped.
#   0. Checks only: 1-Wire must be set up already. /boot/firmware/config.txt
#      needs a dtoverlay=w1-gpio line and /sys/bus/w1/devices a
#      w1_bus_master. This script never edits config.txt and never reboots:
#      it prints the line to add and stops.
#   1. templog (a logger that reads the same probes; only one reader may run):
#      if it is active or enabled, `systemctl disable --now templog`, and the
#      command to bring it back is printed.
#   2. The config: the local file is checked with this commit's tanktemp
#      -check-config (built for this machine) and, if it differs from
#      /etc/tanktemp/config.yaml on the Pi (a diff is shown), installed there
#      as root:root 0644. Without a local file, the Pi's own is kept (and
#      checked); with neither, setup refuses. If tanktemp is already running,
#      it is restarted onto the new config and checked.
#   3. If no build is linked yet, a first one: this commit (clean and pushed,
#      as for deploy.sh), built for the Pi, uploaded as
#      ~/tanktemp/tanktemp-<timestamp>-<commit> with a sha256 check, its
#      -check-config run against the Pi's config, and ~/tanktemp/tanktemp-bin
#      pointed at it. An existing build is left to ./deploy.sh.
#   4. ~/tanktemp/tanktemp.service if there is none, /usr/local/bin/tanktemp
#      -> ~/tanktemp/tanktemp-bin and /etc/systemd/system/tanktemp.service ->
#      ~/tanktemp/tanktemp.service. Anything else already at these paths is
#      refused, not overwritten. Then systemd reloaded, the service enabled and
#      started, and /healthz answering for that build.
#
# Exit: 0 set up (or nothing to do, or --check passed); 1 set up, but the
# service did not come up healthy; 2 a check failed or a step was declined
# (steps already agreed to stay done); 3 a step failed part way: fix it and
# re-run.
#
# Tests: ./deploy_test.sh (runs everything here against stubs).
set -euo pipefail

usage() { sed -n '3,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//p;}' "$0"; }

CHECK=0 YES=0 ALLOW_UNPUSHED=0 REMOTE="" CONFIG=""
while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --yes | -y) YES=1 ;;
        --allow-unpushed) ALLOW_UNPUSHED=1 ;;
        --config)
            [ $# -ge 2 ] || { echo "--config needs a path" >&2; exit 2; }
            CONFIG="$2"
            shift ;;
        --config=*) CONFIG="${1#--config=}" ;;
        -h | --help) usage; exit 0 ;;
        -*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
        *) [ -z "$REMOTE" ] || { echo "only one host, please" >&2; exit 2; }; REMOTE="$1" ;;
    esac
    shift
done
# --config is relative to where this was run from.
CONFIG_GIVEN=0
if [ -n "$CONFIG" ]; then
    CONFIG_GIVEN=1
    case "$CONFIG" in /*) ;; *) CONFIG="$PWD/$CONFIG" ;; esac
fi

cd "$(dirname "${BASH_SOURCE[0]}")"
ACTION=setup
# shellcheck source-path=SCRIPTDIR source=deploy/lib.sh
source deploy/lib.sh
need_remote
[ -n "$CONFIG" ] || CONFIG=deploy/config.local.yaml

stopped() { printf '\nFAILED: %s\nSetup stopped part way. Fix that and re-run: it carries on from here.\n' "$*" >&2; exit 3; }
# shellcheck disable=SC2317,SC2329 # upload_build calls it by name
upload_failed() { stopped "$1 failed"; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --------------------------------------------------------------------------
# What is there already. Read-only, locally and on the Pi.

require_tools

say "checking $REMOTE"
# readlink, or MISSING, or ELSE for something there that is not a symlink.
link_or() { echo "readlink $1 || { [ -e $1 ] && echo ELSE || echo MISSING; }"; }
# state UNIT: is-enabled (not-found if there is no such unit), then is-active.
state() { echo "e=\$(systemctl is-enabled $1 2>/dev/null) || true; echo \"\${e:-not-found}\"; systemctl is-active $1 || true"; }
facts=$(r "uname -m
echo \$HOME
sudo -n true 2>/dev/null && echo sudo-ok || echo sudo-no
if [ -r $BOOT_CONFIG ]; then grep -c '^[[:space:]]*dtoverlay=w1-gpio' $BOOT_CONFIG || true; else echo UNREADABLE; fi
ls -d $W1_DEVICES/w1_bus_master* 2>/dev/null | wc -l
[ -f ~/$PI_DIR/$SERVICE.service ] && echo yes || echo no
$(link_or "\$HOME/$PI_DIR/$LINK")
$(link_or "$BIN_LINK")
$(link_or "$UNIT_LINK")
$(state "$SERVICE")
$(state templog)
[ -f $CONFIG_PATH ] && echo yes || echo no") || refuse "cannot ssh to $REMOTE"
{
    read -r remote_arch
    read -r pi_home
    read -r sudo_ok
    read -r overlays
    read -r masters
    read -r have_unit
    read -r build
    read -r bin_link
    read -r unit_link
    read -r enabled
    read -r active
    read -r templog_enabled
    read -r templog_active
    read -r pi_has_config
} <<<"$facts"

goarch_for "$remote_arch" || refuse "$REMOTE is '$remote_arch', which this script does not build for"
[ "$sudo_ok" = sudo-ok ] || refuse "sudo on $REMOTE needs a password; this script cannot type it"
info "arch $remote_arch, $SERVICE $active"

# 1-Wire: checked, never changed here.
masters="${masters//[[:space:]]/}"
if [ "$overlays" = UNREADABLE ]; then
    refuse "cannot read $BOOT_CONFIG on $REMOTE to check for dtoverlay=w1-gpio"
elif [ "${overlays:-0}" -eq 0 ]; then
    cat >&2 <<EOF

1-Wire is not set up on $REMOTE: $BOOT_CONFIG has no dtoverlay=w1-gpio line.
This script never edits $BOOT_CONFIG and never reboots. Add one line per
bus, under [all], naming the GPIO (BCM numbering) the bus's data wire is on:

    dtoverlay=w1-gpio,gpiopin=4

(a second bus: another line, e.g. dtoverlay=w1-gpio,gpiopin=17). Then reboot
the Pi yourself, check that ls $W1_DEVICES shows a w1_bus_master, and run
this again.
EOF
    refuse "no dtoverlay=w1-gpio in $BOOT_CONFIG"
elif [ "${masters:-0}" -eq 0 ]; then
    refuse "$BOOT_CONFIG has $overlays dtoverlay=w1-gpio line(s), but $W1_DEVICES has no w1_bus_master*: the overlay is not loaded. Reboot the Pi yourself (or check the line) and run this again"
fi
info "1-Wire: $overlays dtoverlay=w1-gpio line(s), $masters bus master(s)"

[ "$build" != ELSE ] || refuse "\$HOME/$PI_DIR/$LINK on $REMOTE is not a symlink (an older layout?); move it aside first"
want_bin="$pi_home/$PI_DIR/$LINK"
want_unit="$pi_home/$PI_DIR/$SERVICE.service"
for pair in "$BIN_LINK|$bin_link|$want_bin" "$UNIT_LINK|$unit_link|$want_unit"; do
    IFS='|' read -r path is want <<<"$pair"
    case "$is" in
        MISSING | "$want") ;;
        ELSE) refuse "$path on $REMOTE is not a symlink; move it aside first" ;;
        *) refuse "$path on $REMOTE points at $is, not $want; move it aside first" ;;
    esac
done

TEMPLOG_BACK="sudo systemctl enable --now templog"
templog_on=0
if [ "$templog_active" = active ] || [ "${templog_enabled#enabled}" != "$templog_enabled" ]; then
    templog_on=1
    info "templog is $templog_active and $templog_enabled: it reads the same probes, and only one reader may run"
fi

# The config: validated here with this commit's tanktemp, whichever is used.
config_check() { # FILE: run -check-config on FILE here; refuse if it fails.
    local out
    [ -n "${HOST_BIN:-}" ] || build_host
    out=$("$HOST_BIN" -check-config -config "$1" 2>&1) || refuse "$2 is not a valid config. tanktemp says:
$out"
    info "$out"
}
CONFIG_CHANGE=0
if [ "$pi_has_config" = yes ]; then
    r "sudo cat $CONFIG_PATH" >"$TMP/pi-config.yaml" || refuse "cannot read $CONFIG_PATH on $REMOTE"
fi
if [ -f "$CONFIG" ]; then
    say "checking $CONFIG"
    config_check "$CONFIG" "$CONFIG"
    if [ "$pi_has_config" = yes ] && cmp -s "$TMP/pi-config.yaml" "$CONFIG"; then
        info "$CONFIG_PATH on $REMOTE is the same"
    else
        CONFIG_CHANGE=1
        if [ "$pi_has_config" = yes ]; then
            info "it differs from $CONFIG_PATH on $REMOTE:"
            diff -u -L "$REMOTE:$CONFIG_PATH" -L "$CONFIG" "$TMP/pi-config.yaml" "$CONFIG" | sed 's/^/        /' || true
        else
            info "$REMOTE has no $CONFIG_PATH yet"
        fi
    fi
elif [ $CONFIG_GIVEN -eq 1 ]; then
    refuse "--config $CONFIG: no such file"
elif [ "$pi_has_config" = yes ]; then
    say "no $CONFIG here: keeping $CONFIG_PATH on $REMOTE"
    config_check "$TMP/pi-config.yaml" "$CONFIG_PATH on $REMOTE"
else
    refuse "no config: $REMOTE has no $CONFIG_PATH and there is no $CONFIG here (copy deploy/config.example.yaml to it, or give --config PATH)"
fi

# The first build, named by its commit, as deploy.sh's are.
if [ "$build" = MISSING ]; then
    say "checking the commit"
    check_commit
fi

todo=()
[ $templog_on -eq 0 ] || todo+=("stop and disable templog (to bring it back: $TEMPLOG_BACK)")
[ $CONFIG_CHANGE -eq 0 ] || todo+=("install $CONFIG as $CONFIG_PATH (root:root 0644)")
install=()
[ "$build" != MISSING ] || install+=("build $VERSION for $remote_arch, upload it, check it accepts $CONFIG_PATH and link ~/$PI_DIR/$LINK to it")
[ "$have_unit" = yes ] || install+=("install the unit file as ~/$PI_DIR/$SERVICE.service")
[ "$bin_link" = "$want_bin" ] || install+=("link $BIN_LINK -> $want_bin")
[ "$unit_link" = "$want_unit" ] || install+=("link $UNIT_LINK -> $want_unit")
[ "$enabled" = enabled ] || install+=("enable $SERVICE")
start=0
if [ "$active" != active ] || [ "$build" = MISSING ]; then
    start=1
    install+=("start $SERVICE and wait for /healthz to answer for it")
elif [ $CONFIG_CHANGE -eq 1 ]; then
    start=2
    install+=("restart $SERVICE onto the new config and wait for /healthz")
fi
todo+=(${install[@]+"${install[@]}"})

if [ ${#todo[@]} -eq 0 ] && [ ${#PROBLEMS[@]} -eq 0 ]; then
    say "$REMOTE is already set up: nothing to do (./deploy.sh from here)"
    exit 0
fi

echo
if [ "$CHECK" -eq 1 ] && [ ${#PROBLEMS[@]} -gt 0 ]; then
    say "check failed. Setup would refuse:"
    for problem in "${PROBLEMS[@]}"; do info "$problem"; done
    info "otherwise it would:"
    for t in "${todo[@]}"; do info "  $t"; done
    exit 2
fi
say "$([ "$CHECK" -eq 1 ] && echo "check passed. Setup would" || echo "setup will"):"
for t in "${todo[@]}"; do info "$t"; done
[ "$CHECK" -eq 0 ] || exit 0

# Everything that can fail here before a change does: the tests and the build.
if [ "$build" = MISSING ]; then
    vet_and_test
    build_for "$VERSION" "$remote_arch"
fi

# --------------------------------------------------------------------------
# Each step only if needed, each asked for. From here a failure stops with
# exit 3, and a "no" with exit 2, keeping what is done.

if [ $templog_on -eq 1 ]; then
    confirm "Stop and disable templog on $REMOTE (sudo systemctl disable --now templog)?"
    say "stopping templog"
    r "sudo systemctl disable --now templog" || stopped "cannot stop templog"
    CHANGED=1
    info "templog is stopped and disabled. To bring it back: $TEMPLOG_BACK"
fi

if [ $CONFIG_CHANGE -eq 1 ]; then
    confirm "Install $CONFIG as $CONFIG_PATH on $REMOTE?"
    say "installing $CONFIG_PATH"
    r "mkdir -p ~/$PI_DIR" || stopped "cannot create ~/$PI_DIR"
    rsync -e "ssh ${SSHOPTS[*]}" "$CONFIG" "$REMOTE:$PI_DIR/.config.yaml.incoming" || stopped "uploading the config failed"
    r "sudo install -d -m 0755 \"\$(dirname $CONFIG_PATH)\" &&
        sudo install -o root -g root -m 0644 ~/$PI_DIR/.config.yaml.incoming $CONFIG_PATH &&
        cmp -s ~/$PI_DIR/.config.yaml.incoming $CONFIG_PATH && rm -f ~/$PI_DIR/.config.yaml.incoming" ||
        stopped "installing $CONFIG_PATH failed"
    CHANGED=1
fi

[ ${#install[@]} -gt 0 ] || { say "$REMOTE is set up"; exit 0; }
if [ $start -eq 2 ] && [ ${#install[@]} -eq 1 ]; then
    confirm "Restart $SERVICE on $REMOTE onto the new config?"
else
    confirm "Install and start $SERVICE on $REMOTE as above?"
fi

r "mkdir -p ~/$PI_DIR" || stopped "cannot create ~/$PI_DIR"
if [ "$have_unit" != yes ]; then
    say "installing the unit file"
    rsync -e "ssh ${SSHOPTS[*]}" "$SERVICE.service" "$REMOTE:$PI_DIR/$SERVICE.service" || stopped "uploading the unit file failed"
    CHANGED=1
fi

if [ "$build" = MISSING ]; then
    build="$SERVICE-$(date +%Y%m%d-%H%M%S)-$VERSION"
    upload_build "$build" upload_failed
    CHANGED=1
    say "checking $CONFIG_PATH with $build"
    if ! pi_check_config "$build"; then
        r "rm -f ~/$PI_DIR/$build" || true
        stopped "$build rejects $CONFIG_PATH on $REMOTE (it has been removed). It said:
$CHECK_OUT"
    fi
    info "$CHECK_OUT"
    r "cd ~/$PI_DIR && ln -sn $build $LINK" || stopped "cannot link ~/$PI_DIR/$LINK to $build"
    info "linked ~/$PI_DIR/$LINK -> $build"
fi

reload=0
if [ "$bin_link" != "$want_bin" ]; then
    say "linking $BIN_LINK"
    r "sudo ln -s '$want_bin' '$BIN_LINK'" || stopped "cannot link $BIN_LINK"
    CHANGED=1
fi
if [ "$unit_link" != "$want_unit" ]; then
    say "linking $UNIT_LINK"
    r "sudo ln -s '$want_unit' '$UNIT_LINK'" || stopped "cannot link $UNIT_LINK"
    CHANGED=1 reload=1
fi
if [ $reload -eq 1 ] || [ "$have_unit" != yes ]; then
    r "sudo systemctl daemon-reload" || stopped "systemctl daemon-reload failed"
fi
started=0
if [ "$enabled" != enabled ] && [ $start -eq 1 ]; then
    say "enabling and starting $SERVICE"
    r "sudo systemctl enable --now $SERVICE" || stopped "$SERVICE did not start: sudo journalctl -u $SERVICE on $REMOTE says why"
    started=1
elif [ "$enabled" != enabled ]; then
    say "enabling $SERVICE"
    r "sudo systemctl enable $SERVICE" || stopped "cannot enable $SERVICE"
fi
if [ $start -ne 0 ] && [ $started -eq 0 ]; then
    say "$([ $start -eq 2 ] && echo restarting || echo starting) $SERVICE"
    r "sudo systemctl restart $SERVICE" || stopped "$SERVICE did not start: sudo journalctl -u $SERVICE on $REMOTE says why"
fi
CHANGED=1
[ $start -ne 0 ] || { say "$REMOTE is set up"; exit 0; }

if wait_healthy "$(version_of "$build")"; then
    [ "$H_STATUS" != degraded ] || warn "$SERVICE is degraded: ${H_DETAIL:-no detail}"
    say "$SERVICE $(version_of "$build") is running on $REMOTE, reading $H_SENSORS probe(s). ./deploy.sh from here"
    [ $templog_on -eq 0 ] || info "templog is off; to bring it back: $TEMPLOG_BACK"
    exit 0
fi
echo >&2
echo "!! $SERVICE is set up on $REMOTE but did not come up healthy within ${VERIFY_TIMEOUT}s: $LAST_REASON" >&2
echo "!! Last answer: ${LAST_BODY:-none}" >&2
r "sudo journalctl -u $SERVICE -n 30 --no-pager -o cat" >&2 || true
exit 1
