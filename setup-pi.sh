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
#   2. The tanktemp system user, if there is none: useradd --system
#      --user-group --no-create-home --home-dir /nonexistent --shell
#      /usr/sbin/nologin tanktemp. It needs no other group: the 1-Wire sysfs
#      files are world-readable.
#   3. The config: the local file is checked with this commit's tanktemp
#      -check-config (built for this machine) and, if it differs from
#      /etc/tanktemp/config.yaml on the Pi (a diff is shown), installed there
#      as root:tanktemp 0640. Without a local file, the Pi's own is kept (and
#      checked); with neither, setup refuses. A config (or its mqtt
#      password_file) with another owner or mode is made root:tanktemp 0640.
#      If tanktemp is already running, a new config restarts it. While it
#      still runs as another user (the old layout's), this step waits for
#      the move's prompt and is done just before the restart, so the old
#      process is never left with a config it could not read again.
#   4. /opt/tanktemp, owned by the deploy user (deploy.sh uploads there
#      without sudo), mode 0755 (tanktemp runs what is there).
#   5. If no build is linked there yet:
#      - on a Pi still on the first install's layout (builds in ~/tanktemp,
#        which tanktemp cannot reach through the 0700 home), its builds and
#        .previous are copied to /opt/tanktemp, keeping their mtimes, and
#        /opt/tanktemp/tanktemp-bin linked to the build that runs now. The
#        old builds' unit files are not copied: they ran as the deploy user.
#        ~/tanktemp itself is left as it is, and the command to remove it is
#        printed.
#      - otherwise a first build: this commit (clean and pushed, as for
#        deploy.sh), built for the Pi, uploaded as
#        /opt/tanktemp/tanktemp-<timestamp>-<commit> with a sha256 check, and
#        /opt/tanktemp/tanktemp-bin pointed at it.
#      Either way the build's -check-config is run as tanktemp against the
#      Pi's config before anything else changes. An existing build is left
#      to ./deploy.sh.
#   6. /opt/tanktemp/tanktemp.service if there is none, /usr/local/bin/tanktemp
#      -> /opt/tanktemp/tanktemp-bin and /etc/systemd/system/tanktemp.service
#      -> /opt/tanktemp/tanktemp.service. Links into ~/tanktemp (the old
#      layout) are repointed; anything else already at these paths is
#      refused, not overwritten. Then systemd reloaded, the service enabled
#      and started, or restarted once if it runs as another user than
#      tanktemp (as the old layout's did), and /healthz answering for that
#      build.
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
# perms PATH: owner:group mode, or MISSING.
perms() { echo "if [ -e $1 ]; then stat -c '%U:%G %a' $1; else echo MISSING; fi"; }
facts=$(r "uname -m
echo \$HOME
id -un
id -gn
sudo -n true 2>/dev/null && echo sudo-ok || echo sudo-no
if [ -r $BOOT_CONFIG ]; then grep -c '^[[:space:]]*dtoverlay=w1-gpio' $BOOT_CONFIG || true; else echo UNREADABLE; fi
ls -d $W1_DEVICES/w1_bus_master* 2>/dev/null | wc -l
id -gn $SVC_USER 2>/dev/null || echo NONE
$(perms "$PI_DIR")
[ -f $PI_DIR/$SERVICE.service ] && echo yes || echo no
$(link_or "$PI_DIR/$LINK")
$(link_or "\$HOME/$OLD_PI_DIR/$LINK")
ls -1 \$HOME/$OLD_PI_DIR 2>/dev/null | grep -c '^$SERVICE-2' || true
$(link_or "$BIN_LINK")
$(link_or "$UNIT_LINK")
$(state "$SERVICE")
v=\$(systemctl show -p NeedDaemonReload --value $SERVICE 2>/dev/null) || true; echo \"\${v:-no}\"
pid=\$(systemctl show -p MainPID --value $SERVICE 2>/dev/null) || true
if [ \"\${pid:-0}\" -gt 0 ] 2>/dev/null; then u=\$(ps -o user:32= -p \$pid) || true; echo \${u:-NONE}; else echo NONE; fi
$(state templog)
$(perms "$CONFIG_PATH")") || refuse "cannot ssh to $REMOTE"
{
    read -r remote_arch
    read -r pi_home
    read -r deploy_user
    read -r deploy_group
    read -r sudo_ok
    read -r overlays
    read -r masters
    read -r user_group
    read -r dir_perms
    read -r have_unit
    read -r build
    read -r old_build
    read -r old_builds
    read -r bin_link
    read -r unit_link
    read -r enabled
    read -r active
    read -r need_reload
    read -r running_as
    read -r templog_enabled
    read -r templog_active
    read -r config_perms
} <<<"$facts"
pi_has_config=no
[ "$config_perms" = MISSING ] || pi_has_config=yes

goarch_for "$remote_arch" || refuse "$REMOTE is '$remote_arch', which this script does not build for"
[ "$sudo_ok" = sudo-ok ] || refuse "sudo on $REMOTE needs a password; this script cannot type it"
info "arch $remote_arch, $SERVICE $active$([ "$running_as" = NONE ] || echo " as $running_as")"

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

# The service user: made here, or there already with its own group.
case "$user_group" in
    NONE | "$SVC_USER") ;;
    *) refuse "the $SVC_USER user on $REMOTE exists but its group is $user_group, not $SVC_USER; fix that by hand first" ;;
esac

# The builds: in PI_DIR, or (the first install's layout) in ~/OLD_PI_DIR, to
# be copied over, or neither, for a first build.
old_dir="$pi_home/$OLD_PI_DIR"
[ "$build" != ELSE ] || refuse "$PI_DIR/$LINK on $REMOTE is not a symlink; move it aside first"
migrate=0
if [ "$build" = MISSING ]; then
    case "$old_build" in
        MISSING) ;;
        ELSE) refuse "$old_dir/$LINK on $REMOTE is not a symlink; move it aside first" ;;
        */*) refuse "$old_dir/$LINK on $REMOTE points at $old_build, not a build beside it; move it aside first" ;;
        *) migrate=1 ;;
    esac
fi
old_builds="${old_builds//[[:space:]]/}"
leftover=""
if [ "$old_build" != MISSING ] || [ "${old_builds:-0}" -gt 0 ]; then
    leftover="$old_dir is left as it is (${old_builds:-0} build(s), from before $PI_DIR); nothing runs from it once $SERVICE runs from $PI_DIR. To remove it: rm -rf $old_dir"
fi

want_bin="$PI_DIR/$LINK"
want_unit="$PI_DIR/$SERVICE.service"
for pair in "$BIN_LINK|$bin_link|$want_bin|$old_dir/$LINK" "$UNIT_LINK|$unit_link|$want_unit|$old_dir/$SERVICE.service"; do
    IFS='|' read -r path is want old <<<"$pair"
    case "$is" in
        MISSING | "$want" | "$old") ;;
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
USED_CONFIG="$TMP/pi-config.yaml"
if [ -f "$CONFIG" ]; then
    USED_CONFIG="$CONFIG"
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

# Who may read what: the config and its password_file, root:SVC_USER 0640.
SECRET_PERMS="root:$SVC_USER 640"
fix_perms=() # paths to chown root:SVC_USER and chmod 0640
[ $CONFIG_CHANGE -eq 1 ] || [ "$config_perms" = "$SECRET_PERMS" ] || fix_perms+=("$CONFIG_PATH|$config_perms")
pw_file=$(sed -n 's/^[[:space:]]*password_file:[[:space:]]*//p' "$USED_CONFIG" | head -1 | sed -e 's/[[:space:]]*#.*//' -e "s/^[\"']//" -e "s/[\"']\$//")
if [ -n "$pw_file" ]; then
    pw_perms=$(r "$(perms "$pw_file")") || refuse "cannot ssh to $REMOTE"
    if [ "$pw_perms" = MISSING ]; then
        warn "the config's password_file $pw_file is not on $REMOTE; put it there, root:$SVC_USER 0640"
    elif [ "$pw_perms" != "$SECRET_PERMS" ]; then
        fix_perms+=("$pw_file|$pw_perms")
    fi
fi

# The first build, named by its commit, as deploy.sh's are.
if [ "$build" = MISSING ] && [ $migrate -eq 0 ]; then
    say "checking the commit"
    check_commit
fi

todo=()
[ $templog_on -eq 0 ] || todo+=("stop and disable templog (to bring it back: $TEMPLOG_BACK)")
[ "$user_group" != NONE ] || todo+=("create the $SVC_USER system user (no home, no login shell)")
[ $CONFIG_CHANGE -eq 0 ] || todo+=("install $CONFIG as $CONFIG_PATH (root:$SVC_USER 0640)")
for fp in ${fix_perms[@]+"${fix_perms[@]}"}; do
    todo+=("make ${fp%%|*} root:$SVC_USER 0640 (it is ${fp#*|})")
done
install=()
want_dir="$deploy_user:$deploy_group 755"
if [ "$dir_perms" = MISSING ]; then
    install+=("create $PI_DIR ($deploy_user:$deploy_group 0755)")
elif [ "$dir_perms" != "$want_dir" ]; then
    install+=("make $PI_DIR $deploy_user:$deploy_group 0755 (it is $dir_perms)")
fi
if [ $migrate -eq 1 ]; then
    install+=("copy the ${old_builds:-0} build(s) in $old_dir to $PI_DIR (and .previous, for --rollback)")
    install+=("link $PI_DIR/$LINK to $old_build, the build running now, and check it accepts $CONFIG_PATH as $SVC_USER")
elif [ "$build" = MISSING ]; then
    install+=("build $VERSION for $remote_arch, upload it, check it accepts $CONFIG_PATH as $SVC_USER and link $PI_DIR/$LINK to it")
fi
[ "$have_unit" = yes ] || install+=("install the unit file as $PI_DIR/$SERVICE.service")
for pair in "$BIN_LINK|$bin_link|$want_bin" "$UNIT_LINK|$unit_link|$want_unit"; do
    IFS='|' read -r path is want <<<"$pair"
    case "$is" in
        "$want") ;;
        MISSING) install+=("link $path -> $want") ;;
        *) install+=("link $path -> $want (now $is)") ;;
    esac
done
reload=0
if [ "$unit_link" != "$want_unit" ] || [ "$have_unit" != yes ] || [ "$need_reload" = yes ]; then
    reload=1
    [ "$unit_link" != "$want_unit" ] || [ "$have_unit" != yes ] ||
        install+=("reload systemd (it has not loaded $SERVICE.service as it is now)")
fi
[ "$enabled" = enabled ] || install+=("enable $SERVICE")
start=0
if [ "$active" != active ] || { [ "$build" = MISSING ] && [ $migrate -eq 0 ]; }; then
    start=1
    install+=("start $SERVICE and wait for /healthz to answer for it")
elif [ "$running_as" != "$SVC_USER" ]; then
    start=2
    install+=("restart $SERVICE as $SVC_USER (it runs as $running_as now) and wait for /healthz")
elif [ $CONFIG_CHANGE -eq 1 ]; then
    start=2
    install+=("restart $SERVICE onto the new config and wait for /healthz")
fi
todo+=(${install[@]+"${install[@]}"})

if [ ${#todo[@]} -eq 0 ] && [ ${#PROBLEMS[@]} -eq 0 ]; then
    [ -z "$leftover" ] || info "$leftover"
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
[ -z "$leftover" ] || info "($leftover)"
[ "$CHECK" -eq 0 ] || exit 0

# Everything that can fail here before a change does: the tests and the build.
if [ "$build" = MISSING ] && [ $migrate -eq 0 ]; then
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

if [ "$user_group" = NONE ]; then
    confirm "Create the $SVC_USER system user on $REMOTE?"
    say "creating the $SVC_USER user"
    r "id $SVC_USER >/dev/null 2>&1 || sudo useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin $SVC_USER" ||
        stopped "cannot create the $SVC_USER user"
    CHANGED=1
fi

# While tanktemp still runs as another user (the old layout's), a config
# made root:SVC_USER 0640 is one it could not read again if it restarted.
# So then the config is changed only once the move is agreed to, just before
# the restart, and under the same prompt.
config_work=0 defer=0
[ $CONFIG_CHANGE -eq 0 ] && [ ${#fix_perms[@]} -eq 0 ] || config_work=1
[ "$active" != active ] || [ "$running_as" = "$SVC_USER" ] || [ "$running_as" = NONE ] || defer=$config_work

# install_config: step 3, installing the config and setting its (and the
# password_file's) owner and mode.
install_config() {
    if [ $CONFIG_CHANGE -eq 1 ]; then
        say "installing $CONFIG_PATH"
        # Staged in the deploy user's home (0700): it may hold secrets.
        stage=".$SERVICE-config.yaml.incoming"
        rsync -e "ssh ${SSHOPTS[*]}" "$CONFIG" "$REMOTE:$stage" || stopped "uploading the config failed"
        r "sudo install -d -m 0755 \"\$(dirname $CONFIG_PATH)\" &&
            sudo install -o root -g $SVC_USER -m 0640 ~/$stage $CONFIG_PATH &&
            sudo cmp -s ~/$stage $CONFIG_PATH && rm -f ~/$stage" ||
            stopped "installing $CONFIG_PATH failed"
        CHANGED=1
    fi
    for fp in ${fix_perms[@]+"${fix_perms[@]}"}; do
        f="${fp%%|*}"
        say "making $f root:$SVC_USER 0640"
        r "sudo chown root:$SVC_USER $f && sudo chmod 0640 $f" || stopped "cannot change the owner of $f"
        CHANGED=1
    done
}

if [ $config_work -eq 1 ] && [ $defer -eq 0 ]; then
    if [ $CONFIG_CHANGE -eq 1 ]; then
        confirm "Install $CONFIG as $CONFIG_PATH on $REMOTE (root:$SVC_USER 0640)$([ ${#fix_perms[@]} -eq 0 ] || echo ", and the rest as above")?"
    else
        confirm "Make the files above root:$SVC_USER 0640 on $REMOTE, so $SVC_USER can read them?"
    fi
    install_config
fi

[ ${#install[@]} -gt 0 ] || { say "$REMOTE is set up"; exit 0; }
if [ $defer -eq 1 ]; then
    if [ $migrate -eq 1 ]; then
        confirm "Make the config readable by $SVC_USER, move $SERVICE on $REMOTE to $PI_DIR and run it as $SVC_USER, as above (one restart)?"
    else
        confirm "Make the config readable by $SVC_USER and restart $SERVICE on $REMOTE as $SVC_USER, as above?"
    fi
    install_config
elif [ $start -eq 2 ] && [ ${#install[@]} -eq 1 ] && [ $CONFIG_CHANGE -eq 1 ]; then
    confirm "Restart $SERVICE on $REMOTE onto the new config?"
elif [ $migrate -eq 1 ]; then
    confirm "Move $SERVICE on $REMOTE to $PI_DIR and run it as $SVC_USER, as above (one restart)?"
else
    confirm "Install and start $SERVICE on $REMOTE as above?"
fi

if [ "$dir_perms" != "$want_dir" ]; then
    say "$([ "$dir_perms" = MISSING ] && echo creating || echo fixing) $PI_DIR"
    r "sudo install -d -o $deploy_user -g $deploy_group -m 0755 $PI_DIR" || stopped "cannot create $PI_DIR"
    CHANGED=1
fi
if [ "$have_unit" != yes ]; then
    say "installing the unit file"
    rsync -e "ssh ${SSHOPTS[*]}" "$SERVICE.service" "$REMOTE:$PI_DIR/$SERVICE.service" || stopped "uploading the unit file failed"
    CHANGED=1
fi

if [ $migrate -eq 1 ]; then
    # Copied under a staging name, so a copy cut short is never taken for a
    # build; cp -p keeps the mtimes deploy.sh prunes by. ~/OLD_PI_DIR is only read.
    say "copying the builds in $old_dir to $PI_DIR"
    r "rm -f $PI_DIR/.incoming-$SERVICE-* && cd ~/$OLD_PI_DIR &&
        for b in \$(ls -1 | grep '^$SERVICE-2'); do
            if [ -f $PI_DIR/\$b ] && cmp -s \$b $PI_DIR/\$b; then continue; fi
            cp -p \$b $PI_DIR/.incoming-\$b && mv $PI_DIR/.incoming-\$b $PI_DIR/\$b || exit 1
            echo \"    copied \$b\"
        done &&
        { [ -e $PI_DIR/.previous ] || [ ! -f .previous ] || cp .previous $PI_DIR/.previous; } &&
        [ -f $PI_DIR/$old_build ]" || stopped "copying the builds to $PI_DIR failed"
    CHANGED=1
    build="$old_build"
    say "checking $CONFIG_PATH with $build, as $SVC_USER"
    pi_check_config "$build" || stopped "$build in $PI_DIR cannot be run by $SVC_USER against $CONFIG_PATH (nothing that runs has changed). It said:
$CHECK_OUT"
    info "$CHECK_OUT"
    r "cd $PI_DIR && ln -sfn $build $LINK" || stopped "cannot link $PI_DIR/$LINK to $build"
    info "linked $PI_DIR/$LINK -> $build"
elif [ "$build" = MISSING ]; then
    build="$SERVICE-$(date +%Y%m%d-%H%M%S)-$VERSION"
    upload_build "$build" upload_failed
    CHANGED=1
    say "checking $CONFIG_PATH with $build, as $SVC_USER"
    if ! pi_check_config "$build"; then
        r "rm -f $PI_DIR/$build" || true
        stopped "$build rejects $CONFIG_PATH on $REMOTE (it has been removed). It said:
$CHECK_OUT"
    fi
    info "$CHECK_OUT"
    r "cd $PI_DIR && ln -sn $build $LINK" || stopped "cannot link $PI_DIR/$LINK to $build"
    info "linked $PI_DIR/$LINK -> $build"
fi

if [ "$bin_link" != "$want_bin" ]; then
    say "linking $BIN_LINK"
    r "sudo ln -sfn '$want_bin' '$BIN_LINK'" || stopped "cannot link $BIN_LINK"
    CHANGED=1
fi
if [ "$unit_link" != "$want_unit" ]; then
    say "linking $UNIT_LINK"
    r "sudo ln -sfn '$want_unit' '$UNIT_LINK'" || stopped "cannot link $UNIT_LINK"
    CHANGED=1
fi
if [ $reload -eq 1 ]; then
    r "sudo systemctl daemon-reload" || stopped "systemctl daemon-reload failed"
    CHANGED=1
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
    say "$SERVICE $(version_of "$build") is running on $REMOTE as $SVC_USER, reading $H_SENSORS probe(s). ./deploy.sh from here"
    [ $templog_on -eq 0 ] || info "templog is off; to bring it back: $TEMPLOG_BACK"
    [ -z "$leftover" ] || info "$leftover"
    exit 0
fi
echo >&2
echo "!! $SERVICE is set up on $REMOTE but did not come up healthy within ${VERIFY_TIMEOUT}s: $LAST_REASON" >&2
echo "!! Last answer: ${LAST_BODY:-none}" >&2
r "sudo journalctl -u $SERVICE -n 30 --no-pager -o cat" >&2 || true
exit 1
