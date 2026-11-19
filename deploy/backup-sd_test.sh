#!/usr/bin/env bash
# shellcheck disable=SC2015 # "test && ok || bad": ok always succeeds, so bad runs only on a failed test
# Tests for deploy/backup-sd.sh. diskutil and sudo are stubs; the "card" is a
# small file of random bytes standing in for /dev/rdiskN. No real disk is read.
#
# Usage: ./deploy/backup-sd_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/backup-sd.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/backup-sd-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
STUBS="$WORK/stubs"
mkdir -p "$STUBS" "$WORK/dev"

CARD_BYTES=3145728 # 3 MiB
head -c "$CARD_BYTES" /dev/urandom >"$WORK/dev/rdisk9"

# diskutil info|list|unmountDisk DISK, shaped like the real output.
# FAKE_LOCATION, FAKE_REMOVABLE, FAKE_NO_BOOTFS and FAKE_SIZE vary it.
cat >"$STUBS/diskutil" <<'EOF'
#!/usr/bin/env bash
echo "diskutil $*" >>"$FAKE_LOG"
case "$1" in
info)
    [ "$2" = disk9 ] || exit 1
    cat <<INFO
   Device Identifier:         disk9
   Part of Whole:             disk9
   Protocol:                  USB
   Disk Size:                 3.1 MB (${FAKE_SIZE:-3145728} Bytes) (exactly 6144 512-Byte-Units)
   Device Location:           ${FAKE_LOCATION:-External}
   Removable Media:           ${FAKE_REMOVABLE:-Removable}
INFO
    ;;
list)
    echo "/dev/disk9 (external, physical):"
    echo "   0:     FDisk_partition_scheme                        *3.1 MB     disk9"
    if [ -z "${FAKE_NO_BOOTFS:-}" ]; then
        echo "   1:             Windows_FAT_32 bootfs                  1.0 MB     disk9s1"
    fi
    echo "   2:                      Linux                         2.1 MB     disk9s2"
    ;;
unmountDisk) ;;
*) exit 1 ;;
esac
EOF
cat >"$STUBS/sudo" <<'EOF'
#!/usr/bin/env bash
echo "sudo $*" >>"$FAKE_LOG"
exec "$@"
EOF
chmod +x "$STUBS"/*

export PATH="$STUBS:$PATH" BACKUP_SD_DEV_DIR="$WORK/dev" BACKUP_SD_STAMP=20261119
export FAKE_LOG="$WORK/log"

PASS=0 FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   $1"; }
# shellcheck disable=SC2001 # indents every line of a multi-line string
bad() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -z "${2:-}" ] || sed 's/^/     /' <<<"$2"; }
# run NAME WANT_EXIT ARGS...: runs the script, leaves its output in $OUT_TXT
run() {
    local name=$1 want=$2 got=0
    shift 2
    : >"$FAKE_LOG"
    OUT_TXT=$("$SCRIPT" "$@" 2>&1 </dev/null) || got=$?
    if [ "$got" = "$want" ]; then ok "$name (exit $got)"; else bad "$name: exit $got, want $want" "$OUT_TXT"; fi
}
expect_out() { if grep -q -- "$2" <<<"$OUT_TXT"; then ok "$1"; else bad "$1: no '$2' in output" "$OUT_TXT"; fi; }
expect_no_log() { if grep -q -- "$2" "$FAKE_LOG"; then bad "$1: '$2' was called" "$(cat "$FAKE_LOG")"; else ok "$1"; fi; }

OUTD="$WORK/out"
IMG="$OUTD/tanktemp-sd-20261119.img.gz"

run "no disk given" 1
expect_out "names the fix" "diskutil list external"
run "partition, not whole disk" 1 disk9s1 --out "$OUTD"
run "/dev path refused" 1 /dev/disk9 --out "$OUTD"
run "unknown disk" 1 disk8 --out "$OUTD"
expect_out "says no such disk" "no such disk"

FAKE_LOCATION=Internal run "internal disk refused" 1 disk9 --out "$OUTD"
expect_no_log "internal: nothing unmounted" unmountDisk
FAKE_REMOVABLE=Fixed run "non-removable refused" 1 disk9 --out "$OUTD"
FAKE_NO_BOOTFS=1 run "no bootfs refused" 1 disk9 --out "$OUTD"
expect_out "says not a Pi card" "not a Raspberry Pi OS card"

run "--check" 0 --check disk9 --out "$OUTD"
expect_out "--check says what it would do" "would unmount disk9"
expect_no_log "--check: nothing unmounted" unmountDisk
expect_no_log "--check: nothing read" "sudo"
[ ! -e "$OUTD" ] && ok "--check: no output dir made" || bad "--check made $OUTD"

run "no --yes and no answer stops" 1 disk9 --out "$OUTD"
expect_no_log "unanswered: nothing unmounted" unmountDisk
[ ! -e "$IMG" ] && ok "unanswered: no image" || bad "unanswered: image written"

FAKE_SIZE=4194304 run "short read fails" 1 --yes disk9 --out "$OUTD"
expect_out "says incomplete" "incomplete read"
[ ! -e "$IMG" ] && [ ! -e "$IMG.partial" ] && ok "short read: no image, no partial" || bad "short read left files" "$(ls -la "$OUTD")"

run "--yes images the card" 0 --yes disk9 --out "$OUTD"
expect_out "says done" "done: $IMG"
if gzip -dc "$IMG" | cmp -s - "$WORK/dev/rdisk9"; then ok "image decompresses to the card's exact bytes"; else bad "image differs from the card"; fi
(cd "$OUTD" && shasum -a 256 -c "$(basename "$IMG").sha256" >/dev/null 2>&1) && ok "checksum file verifies" || bad "checksum file does not verify" "$(cat "$IMG.sha256")"
grep -q "unmountDisk disk9" "$FAKE_LOG" && ok "unmounted the card first" || bad "did not unmount" "$(cat "$FAKE_LOG")"
grep -q "sudo dd if=$WORK/dev/rdisk9 " "$FAKE_LOG" && ok "read the raw device with sudo dd" || bad "no sudo dd of the raw device" "$(cat "$FAKE_LOG")"
[ ! -e "$IMG.partial" ] && ok "no .partial left" || bad ".partial left behind"

run "re-run is a no-op" 0 --yes disk9 --out "$OUTD"
expect_out "says it exists" "already exists"
expect_no_log "re-run: nothing read" "sudo"

FULL="$WORK/full"
mkdir -p "$FULL"
# df on a real path, but the card claims to be bigger than any disk.
FAKE_SIZE=999999999999999 run "not enough space refused" 1 --check disk9 --out "$FULL"
expect_out "says free space" "GiB free"

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
