#!/usr/bin/env bash
# Image a Pi's whole SD card, from this Mac's card reader, to a gzipped file.
#
# Usage: ./deploy/backup-sd.sh [--check] [--yes] [--out DIR] diskN
#
#   diskN     the card's whole-disk identifier, from `diskutil list external`
#   --check   check everything and say what would happen; change nothing
#   --yes     don't ask before unmounting the card and reading it
#   --out DIR where the image goes (default ~/tanktemp-backups)
#
# Writes DIR/tanktemp-sd-YYYYMMDD.img.gz and its .sha256. The image holds the
# Pi's secrets (wifi, ssh keys, config), so it lives outside the repo.
# Read-only on the card: it is unmounted, then read with `sudo dd`, never
# written. Re-running when today's image exists does nothing. A failed or
# interrupted read leaves no image behind. Restore with Raspberry Pi Imager
# ("Use custom") or: gzip -dc FILE | sudo dd of=/dev/rdiskN bs=4m
#
# Tests: ./deploy/backup-sd_test.sh
set -euo pipefail

DEV_DIR="${BACKUP_SD_DEV_DIR:-/dev}" # tests point this at a fake
STAMP="${BACKUP_SD_STAMP:-$(date +%Y%m%d)}"
OUT_DIR="$HOME/tanktemp-backups"
CHECK=0 YES=0 DISK=""

die() { echo "backup-sd: $*" >&2; exit 1; }
info() { echo "backup-sd: $*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --check | --dry-run) CHECK=1 ;;
        --yes) YES=1 ;;
        --out) [ $# -ge 2 ] || die "--out needs a directory"; OUT_DIR="$2"; shift ;;
        -h | --help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*) die "unknown option $1 (try --help)" ;;
        *) [ -z "$DISK" ] || die "only one disk, please"; DISK="$1" ;;
    esac
    shift
done

[ -n "$DISK" ] || die "which disk? Run 'diskutil list external' and pass e.g. disk4"
[[ "$DISK" =~ ^disk[0-9]+$ ]] || die "'$DISK' is not a whole disk: pass e.g. disk4, not /dev/disk4 or disk4s1"

# --- Preconditions (read-only) ---------------------------------------------
INFO=$(diskutil info "$DISK" 2>/dev/null) || die "$DISK: no such disk"
field() { sed -n "s/^ *$1: *//p" <<<"$INFO" | head -1; }
[ "$(field 'Part of Whole')" = "$DISK" ] || die "$DISK is not a whole disk"
[ "$(field 'Device Location')" = External ] || die "$DISK is not an external disk; refusing"
[ "$(field 'Removable Media')" = Removable ] || die "$DISK is not removable media; refusing"
SIZE=$(field 'Disk Size' | sed -n 's/.*(\([0-9]*\) Bytes).*/\1/p')
[[ "$SIZE" =~ ^[0-9]+$ ]] && [ "$SIZE" -gt 0 ] || die "$DISK: cannot read its size"

LAYOUT=$(diskutil list "$DISK")
grep -qE 'Windows_FAT_32 +bootfs' <<<"$LAYOUT" || die "$DISK has no 'bootfs' partition: not a Raspberry Pi OS card?"
grep -qE ' Linux ' <<<"$LAYOUT" || die "$DISK has no Linux partition: not a Raspberry Pi OS card?"
[ -r "$DEV_DIR/r$DISK" ] || [ -e "$DEV_DIR/r$DISK" ] || die "$DEV_DIR/r$DISK does not exist"

OUT="$OUT_DIR/tanktemp-sd-$STAMP.img.gz"
GB=$(awk -v b="$SIZE" 'BEGIN { printf "%.1f", b / 1e9 }')
info "card: $DISK, $GB GB, external removable, Raspberry Pi OS layout (bootfs + Linux)"

if [ -e "$OUT" ]; then
    info "$OUT already exists: nothing to do (move it aside to take another)"
    exit 0
fi

# Free space: assume the worst case, an image as big as the card.
if [ -d "$OUT_DIR" ]; then
    FREE_K=$(df -Pk "$OUT_DIR" | awk 'NR == 2 { print $4 }')
    [ $((FREE_K * 1024)) -gt "$SIZE" ] || die "$OUT_DIR has $((FREE_K / 1048576)) GiB free; the card is $GB GB"
fi

if [ "$CHECK" = 1 ]; then
    info "would unmount $DISK and read all of $DEV_DIR/r$DISK with sudo dd into $OUT"
    info "--check: nothing changed"
    exit 0
fi

if [ "$YES" != 1 ]; then
    printf 'backup-sd: about to unmount %s and read all %s GB of it (sudo, read-only) into\n  %s\nContinue? [y/N] ' "$DISK" "$GB" "$OUT"
    read -r answer || answer=""
    case "$answer" in y | Y | yes) ;; *) info "stopped; nothing changed"; exit 1 ;; esac
fi

# --- Image it ---------------------------------------------------------------
mkdir -p "$OUT_DIR"
PARTIAL="$OUT.partial"
trap 'rm -f "$PARTIAL"; echo "backup-sd: failed; no image written" >&2' ERR INT TERM

diskutil unmountDisk "$DISK" >/dev/null || die "could not unmount $DISK (is something using it?)"
info "reading $DISK; sudo may ask for your password. Takes a while: about 15-30 min"
sudo dd if="$DEV_DIR/r$DISK" bs=4m status=progress | gzip -1 >"$PARTIAL"

info "verifying: the image must decompress to exactly $SIZE bytes"
GOT=$(gzip -dc "$PARTIAL" | wc -c | tr -d ' ')
if [ "$GOT" != "$SIZE" ]; then
    rm -f "$PARTIAL"
    die "read $GOT bytes but the card is $SIZE: incomplete read, no image written"
fi
(cd "$OUT_DIR" && shasum -a 256 "$(basename "$PARTIAL")" | sed 's/\.partial$//') >"$OUT.sha256"
mv "$PARTIAL" "$OUT"
trap - ERR INT TERM

info "done: $OUT ($(du -h "$OUT" | cut -f1 | tr -d ' '))"
info "checksum: $OUT.sha256. The card is unmounted; eject it from Finder or with: diskutil eject $DISK"
