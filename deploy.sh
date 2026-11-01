#!/usr/bin/env bash
#
# Build, deploy and verify tanktemp on its Pi, rolling back on its own if the
# new build does not come up healthy.
#
#   ./deploy.sh --check          read-only: run every pre-flight check, change nothing
#   ./deploy.sh                  deploy (asks before changing anything)
#   ./deploy.sh --yes            deploy without asking
#   ./deploy.sh --allow-unhealthy  deploy a fix over a running build that is not
#                                healthy (refused otherwise); its rollback then
#                                accepts the old build back as it was
#   ./deploy.sh --allow-dirty    deploy uncommitted changes, as <commit>-dirty
#   ./deploy.sh --allow-unpushed deploy a commit that is not pushed (or with no
#                                git remote at all): a warning, not a refusal
#   ./deploy.sh --rollback       switch back to the build that ran before the last deploy
#   ./deploy.sh --check --rollback   read-only: what --rollback would do
#   ./deploy.sh [opts] user@host the Pi; without it, the one line of deploy/host
#                                (gitignored). The repo names no host.
#
# What a deploy does:
#   1. Pre-flight, all read-only: the Pi reachable with passwordless sudo, the
#      service, its config and its symlinks as expected (a Pi still on the
#      first install's ~/tanktemp layout is refused: ./setup-pi.sh moves it), the running build
#      healthy (/healthz: ready, MQTT connected, not "starting"; "degraded" is
#      a warning) and how many probes it reads, the broker's retained status
#      readable from here, a clean, pushed commit (the version is the commit
#      hash, so its date must be final), `go vet` and `go test`.
#   2. Build for the Pi's architecture (through go.work), stamped with the commit.
#   3. Upload to /opt/tanktemp (the deploy user's, so no sudo) under a new
#      name and check the sha256. Run the new build's -check-config, as the
#      tanktemp user, against /etc/tanktemp/config.yaml: if it rejects it,
#      the upload is removed and nothing else happens. Only then switch the
#      symlink /opt/tanktemp/tanktemp-bin (what the unit runs) and restart.
#      The old build stays on disk, and its name is kept in
#      /opt/tanktemp/.previous for --rollback. Each build's unit file is kept
#      with it, in /opt/tanktemp/.units/<build>.service, and a switch to a
#      build (forward or back) installs its unit first if it differs.
#   4. Verify: /healthz reports this version, ready, MQTT connected, not
#      "starting", and no fewer probes reading (sensors_ok over all tanks)
#      than before; it stays up; the journal shows no panic or fatal error;
#      and the broker's retained {prefix}/status is a HEARTBEAT, online, from
#      this version (what statehouse sees).
#   5. If any of 4 fails, switch back to the previous build and its unit file,
#      restart it and check it is healthy again. If installing the new unit
#      file fails, the old one is put back and nothing is restarted (exit 2).
#
# --check goes through every check and exits 2 if a deploy would refuse.
#
# Exit: 0 deployed and verified (or --check passed); 1 the new build failed
# to start or to verify and was rolled back; 2 a pre-flight check failed, the
# new build rejected the config, the link to the Pi dropped before the
# switch, or the deploy was declined: nothing that runs changed; 3 the
# rollback did not come up healthy (or could not be started): needs a person.
#
# Tests: ./deploy_test.sh (runs everything here against stubs).
set -euo pipefail

KEEP=3
SETTLE="${DEPLOY_SETTLE:-5}"

usage() { sed -n '3,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//p;}' "$0"; }

MODE=deploy CHECK=0 YES=0 ALLOW_DIRTY=0 ALLOW_UNPUSHED=0 ALLOW_UNHEALTHY=0 REMOTE=""
while [ $# -gt 0 ]; do
    case "$1" in
        --check) CHECK=1 ;;
        --rollback) MODE=rollback ;;
        --yes | -y) YES=1 ;;
        --allow-dirty) ALLOW_DIRTY=1 ;;
        --allow-unpushed) ALLOW_UNPUSHED=1 ;;
        --allow-unhealthy) ALLOW_UNHEALTHY=1 ;;
        -h | --help) usage; exit 0 ;;
        -*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
        *) [ -z "$REMOTE" ] || { echo "only one host, please" >&2; exit 2; }; REMOTE="$1" ;;
    esac
    shift
done

cd "$(dirname "${BASH_SOURCE[0]}")"
ACTION=deploy
# shellcheck source-path=SCRIPTDIR source=deploy/lib.sh
source deploy/lib.sh
need_remote

# switch_to NAME: point the service at build NAME and restart it.
switch_to() {
    r "cd $PI_DIR && ln -sfn '$1' $LINK && sudo systemctl restart $SERVICE"
}

# install_unit NAME: make NAME's unit file the installed one, if it has one
# that differs, and have systemd reload it.
install_unit() {
    r "cd $PI_DIR && if [ -f $UNITS/$1.service ] && ! cmp -s $UNITS/$1.service $SERVICE.service; then
        cp $UNITS/$1.service .$SERVICE.service.tmp && mv .$SERVICE.service.tmp $SERVICE.service &&
        sudo systemctl daemon-reload && echo '    installed the unit file of $1'
    fi"
}

# needs_person WHAT: stop, with what is known, for someone to look at the Pi.
needs_person() {
    echo >&2
    echo "!! $1. $SERVICE needs a person now." >&2
    echo "!! Last answer: ${LAST_BODY:-none}" >&2
    r "systemctl status $SERVICE --no-pager -n 0; sudo journalctl -u $SERVICE -n 30 --no-pager -o cat" >&2 || true
    exit 3
}

# rollback_to NAME [AS_IT_WAS]: switch back to NAME and see it healthy (its
# version, ready, MQTT; the probe count is not held against it). With
# AS_IT_WAS=1 (it was not healthy before the deploy either), NAME running
# again is as good as it gets.
rollback_to() {
    local prev="$1" as_it_was="${2:-0}"
    say "rolling back to $prev"
    LAST_BODY=""
    if install_unit "$prev" && switch_to "$prev"; then
        if wait_healthy "$(version_of "$prev")"; then
            info "$prev is running and healthy again"
            return 0
        elif [ "$as_it_was" -eq 1 ]; then
            warn "$prev is back, but not healthy: it was not healthy before the deploy either ($LAST_REASON)"
            return 0
        fi
    fi
    needs_person "ROLLBACK DID NOT COME UP HEALTHY"
}

# probe_broker: can the result be checked end to end, the way statehouse sees
# it? Sets BROKER_CHECK from CFG_BROKER and CFG_PREFIX.
BROKER_CHECK=0 BROKER_HOST=""
probe_broker() {
    local hb
    BROKER_CHECK=0 BROKER_HOST=""
    if ! command -v mosquitto_sub >/dev/null; then
        warn "no mosquitto_sub here; the broker check will be skipped"
    elif [ -z "${CFG_BROKER:-}" ]; then
        warn "no broker address from -check-config; the broker check will be skipped"
    elif ! broker_addr "$CFG_BROKER"; then
        warn "cannot read $CFG_BROKER with mosquitto_sub (TLS?); the broker check will be skipped"
    elif hb=$(retained_status) && [ -n "$hb" ]; then
        BROKER_CHECK=1
        info "broker $BROKER_HOST:$BROKER_PORT: retained $CFG_PREFIX/status is $(jget event <<<"$hb") from $(jget version <<<"$hb")"
    else
        warn "cannot read the retained $CFG_PREFIX/status from $BROKER_HOST:$BROKER_PORT; the broker check will be skipped"
    fi
}

# --------------------------------------------------------------------------
# Pre-flight. Everything here is read-only, locally and on the Pi.

require_tools

say "checking $REMOTE"
remote_facts=$(r "uname -m
readlink $PI_DIR/$LINK || echo MISSING
readlink $BIN_LINK || echo MISSING
readlink $UNIT_LINK || echo MISSING
[ -L ~/$OLD_PI_DIR/$LINK ] && echo old-layout || echo no-old-layout
systemctl is-active $SERVICE || true
sudo -n true 2>/dev/null && echo sudo-ok || echo sudo-no
{ df -Pk $PI_DIR 2>/dev/null || true; } | awk 'NR==2 {v = \$4} END {print v + 0}'
cat $PI_DIR/.previous 2>/dev/null || echo NONE
[ -f $CONFIG_PATH ] && echo config-yes || echo config-no
systemctl is-active templog 2>/dev/null || true
ls -1t $PI_DIR 2>/dev/null | grep '^$SERVICE-2' || true") || refuse "cannot ssh to $REMOTE"

{
    read -r remote_arch
    read -r CURRENT
    read -r bin_link
    read -r unit_link
    read -r old_layout
    read -r active
    read -r sudo_ok
    read -r free_kb
    read -r recorded_prev
    read -r have_config
    read -r templog
    builds=$(cat)
} <<<"$remote_facts"

goarch_for "$remote_arch" || refuse "$REMOTE is '$remote_arch', which this script does not build for"
info "arch $remote_arch, service $active"
[ "$sudo_ok" = sudo-ok ] || refuse "sudo on $REMOTE needs a password; this script cannot type it"
if [ "$CURRENT" = MISSING ] && [ "$old_layout" = old-layout ]; then
    refuse "$REMOTE still has its builds in ~/$OLD_PI_DIR, where the $SVC_USER user cannot reach them: run ./setup-pi.sh to move them to $PI_DIR"
fi
[ "$CURRENT" != MISSING ] || refuse "$PI_DIR/$LINK is missing: this host was never set up (run ./setup-pi.sh)"
[ "$unit_link" = "$PI_DIR/$SERVICE.service" ] ||
    refuse "$UNIT_LINK points at '$unit_link', not $PI_DIR/$SERVICE.service, so a new unit file would not be used (run ./setup-pi.sh)"
[ "$bin_link" = "$PI_DIR/$LINK" ] ||
    refuse "$BIN_LINK points at '$bin_link', not $PI_DIR/$LINK (run ./setup-pi.sh)"
[ "$have_config" = config-yes ] || refuse "$CONFIG_PATH is missing on $REMOTE (run ./setup-pi.sh to install it)"
[ "${free_kb:-0}" -gt 51200 ] || refuse "less than 50MB free in $PI_DIR on $REMOTE"
[ "$templog" != active ] || warn "templog is active on $REMOTE: it reads the same probes (./setup-pi.sh stops it)"
info "running build: $CURRENT"

# The running build's view of the installed config: the broker and prefix to
# check, until the new build gives its own.
CFG_BROKER="" CFG_PREFIX=""
if pi_check_config "$LINK"; then
    info "$CHECK_OUT"
    parse_check_config "$CHECK_OUT" || warn "cannot read the broker from: $CHECK_OUT"
else
    warn "the running build rejects $CONFIG_PATH: $CHECK_OUT"
fi

# unhealthy WHY: a deploy's rollback expects the old build back as healthy as
# it was, so with the broker down, say, a deploy can only fail and its
# rollback be judged failed too. Refused, unless this is a fix for the broken
# build (--allow-unhealthy), when the rollback accepts it back as it was. A
# rollback goes ahead: this may be why it is wanted.
PRE_HEALTHY=1
unhealthy() {
    PRE_HEALTHY=0
    if [ "$MODE" = rollback ]; then
        warn "$1"
    elif [ "$ALLOW_UNHEALTHY" -eq 1 ]; then
        warn "$1; deploying anyway (--allow-unhealthy)"
    elif [ "$CHECK" -eq 1 ]; then
        would_refuse "$1 (--allow-unhealthy to deploy a fix over it)"
    else
        refuse "$1. Deploy once it is, or deploy a fix over it with --allow-unhealthy"
    fi
}

# The state now, to compare with after.
PRE_SENSORS=""
if pre_body=$(http /healthz); then
    health_of "$pre_body"
    PRE_SENSORS="$H_SENSORS"
    info "/healthz: $H_STATUS, version ${H_VERSION:-?}, ready=$H_READY mqtt=$H_MQTT, sensors_ok ${H_SENSORS:-none}"
    why=$(unhealthy_reason)
    if [ -n "$why" ]; then
        unhealthy "the running build is not healthy right now: $why"
    elif [ "$H_STATUS" = degraded ]; then
        warn "the running build is degraded: ${H_DETAIL:-no detail}"
    fi
else
    unhealthy "the running build is not healthy right now: http://$HTTP_HOST/healthz did not answer"
fi

if [ "$MODE" = rollback ]; then
    [ "$recorded_prev" != NONE ] || refuse "no $PI_DIR/.previous: nothing recorded to roll back to"
    grep -qx "$recorded_prev" <<<"$builds" || refuse "the recorded previous build $recorded_prev is no longer on $REMOTE"
    [ "$recorded_prev" != "$CURRENT" ] || refuse "$recorded_prev is already running"
    if [ "$CHECK" -eq 1 ]; then
        echo
        say "check passed. A rollback would switch $REMOTE from $CURRENT back to $recorded_prev and restart $SERVICE"
        exit 0
    fi
    confirm "Switch $REMOTE from $CURRENT back to $recorded_prev and restart $SERVICE?"
    r "printf '%s\n' '$CURRENT' > $PI_DIR/.previous" || refuse "cannot record $CURRENT as the build to come back to"
    rollback_to "$recorded_prev"
    say "$SERVICE is back on $recorded_prev (./deploy.sh --rollback again to undo)"
    exit 0
fi

probe_broker

say "checking the commit"
check_commit

vet_and_test

if [ "$CHECK" -eq 1 ]; then
    echo
    if [ ${#PROBLEMS[@]} -gt 0 ]; then
        say "check failed. A deploy would refuse:"
        for problem in "${PROBLEMS[@]}"; do info "$problem"; done
        exit 2
    fi
    say "check passed. A deploy would:"
    info "build $VERSION for $remote_arch and upload it as $SERVICE-<timestamp>-$VERSION"
    info "check it accepts $CONFIG_PATH"
    info "switch $PI_DIR/$LINK from $CURRENT and restart $SERVICE (as $SVC_USER)"
    info "verify /healthz (sensors_ok at least ${PRE_SENSORS:-0}), the journal$([ $BROKER_CHECK -eq 1 ] && echo ' and the retained status'), or roll back"
    exit 0
fi

# --------------------------------------------------------------------------
# Build and upload. Nothing that runs changes until the switch.

build_for "$VERSION" "$remote_arch"

NEW="$SERVICE-$(date +%Y%m%d-%H%M%S)-$VERSION"
confirm "Upload $NEW to $REMOTE, switch $SERVICE to it from $CURRENT and restart?"

STAGE=".incoming-$NEW"
cleanup_stage() { r "rm -f $PI_DIR/$STAGE $PI_DIR/$NEW $PI_DIR/$UNITS/$NEW.service" 2>/dev/null || true; }
# step_failed STEP: a step after the build, before anything that runs
# changed, failed. Say whether the link dropped or the step itself failed (a
# full disk, say), and tidy up if the link is there; a later deploy removes
# what this can't.
step_failed() {
    if r true 2>/dev/null; then
        cleanup_stage
        refuse "$1 failed on $REMOTE"
    fi
    refuse "lost $REMOTE while $1"
}
upload_build "$NEW" step_failed

# The new build must accept the config it will run with.
say "checking $CONFIG_PATH with $NEW"
crc=0
pi_check_config "$NEW" || crc=$?
if [ $crc -eq 255 ]; then
    step_failed "running $NEW -check-config"
elif [ $crc -ne 0 ]; then
    cleanup_stage
    refuse "$NEW rejects $CONFIG_PATH on $REMOTE, so it would not start. It has been removed. It said:
$CHECK_OUT"
fi
info "$CHECK_OUT"
old_broker="$CFG_BROKER" old_prefix="$CFG_PREFIX"
parse_check_config "$CHECK_OUT" || { CFG_BROKER="" CFG_PREFIX=""; }
if [ "$CFG_BROKER" != "$old_broker" ] || [ "$CFG_PREFIX" != "$old_prefix" ]; then
    probe_broker
fi

since=$(r "date +%s") || step_failed "reading its clock"

# The unit file the running build runs under, and the new build's, beside
# their builds. Nothing that runs reads these.
r "mkdir -p $PI_DIR/$UNITS && cd $PI_DIR && { [ ! -f $SERVICE.service ] || cp $SERVICE.service $UNITS/$CURRENT.service; }" ||
    step_failed "recording the running build's unit file"
rsync -e "ssh ${SSHOPTS[*]}" "$SERVICE.service" "$REMOTE:$PI_DIR/$UNITS/$NEW.service" || step_failed "uploading the unit file"

# put_back_unit: after a failed install of the new unit, nothing has restarted:
# put the running build's unit back (or none, if it had none here).
put_back_unit() {
    r "cd $PI_DIR && if [ -f $UNITS/$CURRENT.service ]; then
        cp $UNITS/$CURRENT.service .$SERVICE.service.tmp && mv .$SERVICE.service.tmp $SERVICE.service
    else rm -f $SERVICE.service; fi && { sudo systemctl daemon-reload || true; }"
}

# --------------------------------------------------------------------------
# Install the unit if changed, switch, restart, verify.

if ! install_unit "$NEW"; then
    put_back_unit || needs_person "installing the unit file of $NEW failed, and so did putting the old one back"
    cleanup_stage
    refuse "installing the unit file of $NEW failed (systemctl daemon-reload, or the link). The old unit file is back"
fi

failed=""
if ! { say "switching to $NEW and restarting" &&
    r "printf '%s\n' '$CURRENT' > $PI_DIR/.previous" && switch_to "$NEW"; }; then
    failed="the restart failed"
elif ! wait_healthy "$VERSION" "$PRE_SENSORS"; then
    failed="/healthz was not healthy within ${VERIFY_TIMEOUT}s: $LAST_REASON (last: ${LAST_BODY:-no answer})"
else
    info "/healthz: $VERSION $H_STATUS, ready, MQTT connected, sensors_ok $H_SENSORS"
    start_time="$H_START"
    # Still the same process a few seconds later? A crash loop restarts under
    # systemd and would show a new start_time.
    sleep "$SETTLE"
    later="" why=""
    if later=$(http /healthz); then
        health_of "$later"
        why=$(unhealthy_reason "$VERSION" "$PRE_SENSORS")
    fi
    if [ -z "$later" ] || [ "$H_START" != "$start_time" ] || [ -n "$why" ]; then
        failed="it did not stay up: ${later:-no answer}"
    fi
fi

# The new process's own output only. sudo: the deploy user may not read the
# system journal, and an unread journal must not pass.
if [ -z "$failed" ]; then
    if ! journal=$(r "inv=\$(systemctl show -p InvocationID --value $SERVICE) && [ -n \"\$inv\" ] &&
        sudo journalctl _SYSTEMD_INVOCATION_ID=\$inv --no-pager -o cat"); then
        failed="cannot read the journal of $NEW"
    elif grep -qiE 'fatal|panic' <<<"$journal"; then
        failed="the journal shows: $(grep -iE 'fatal|panic' <<<"$journal" | head -3)"
    fi
fi

# The retained status, as statehouse sees it: the new process publishes a
# HEARTBEAT once its first sweep is out, which is when /healthz says ready.
if [ -z "$failed" ] && [ $BROKER_CHECK -eq 1 ]; then
    deadline=$((SECONDS + BROKER_TIMEOUT))
    while :; do
        hb=$(retained_status) || hb=""
        if [ "$(jget event <<<"$hb")" = HEARTBEAT ] && [ "$(jget online <<<"$hb")" = true ] &&
            [ "$(jget version <<<"$hb")" = "$VERSION" ]; then
            info "broker holds a HEARTBEAT from $VERSION on $CFG_PREFIX/status"
            break
        fi
        if [ $SECONDS -ge $deadline ]; then
            failed="the broker's retained $CFG_PREFIX/status is not a HEARTBEAT from $VERSION: ${hb:-nothing}"
            break
        fi
        sleep "$POLL"
    done
fi

if [ -n "$failed" ]; then
    echo >&2
    echo "!! $NEW FAILED VERIFICATION: $failed" >&2
    r "sudo journalctl -u $SERVICE --since @$since --no-pager -o cat" 2>/dev/null | tail -20 | sed 's/^/   | /' >&2 || true
    r "printf '%s\n' '$NEW' > $PI_DIR/.previous" || warn "could not record $NEW in .previous"
    rollback_to "$CURRENT" $((1 - PRE_HEALTHY))
    echo "$NEW stays on $REMOTE for a look; the next deploy's cleanup removes it." >&2
    exit 1
fi

health_of "$LAST_BODY"
[ "$H_STATUS" != degraded ] || warn "$NEW is degraded: ${H_DETAIL:-no detail}"

# Keep the newest $KEEP builds, and never the one just switched away from.
# Uploads that a dropped link left staged go too, and the unit files of
# builds no longer there.
r "cd $PI_DIR && rm -f .incoming-$SERVICE-* && ls -1t | grep '^$SERVICE-2' | tail -n +$((KEEP + 1)) | grep -vx -e '$NEW' -e '$CURRENT' | xargs -r rm -f -- &&
    for u in $UNITS/*.service; do [ ! -f \"\$u\" ] || [ -e \"\$(basename \"\$u\" .service)\" ] || rm -f \"\$u\"; done" ||
    warn "could not remove old builds from $REMOTE; the next deploy will"

echo
say "$SERVICE $VERSION is running on $REMOTE"
r "sudo journalctl -u $SERVICE --since @$since --no-pager -o cat" | tail -8 | sed 's/^/    /' || true
info "to undo: ./deploy.sh --rollback (back to $CURRENT)"
