#!/usr/bin/env python3
"""Generate the onewire scenario fixtures' expect.json files and the synthetic
scenarios.

Real captures (rig-*) are made on the Pi with cmd/w1capture and copied here
unchanged. Their expected values come from the kernel's own t= line, not from
our parser, so the test compares our decoding with the kernel's.

Synthetic scenarios are built from rig-steady by changing one thing, using
the byte patterns documented in docs/ONEWIRE.md. Each is marked
"synthetic": true and should be replaced by a real capture when one exists.

    python3 internal/onewire/testdata/gen_fixtures.py

It rewrites every synthetic-* directory and every expect.json; real capture
trees are only read.
"""
import json
import os
import shutil

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "sysfs")
BASE = os.path.join(ROOT, "rig-steady")
ATTEMPTS = 3
GHOST = "28-0000deadbeef"  # on no bus; requested in every scenario


def crc8(data):
    crc = 0
    for b in data:
        for _ in range(8):
            mix = (crc ^ b) & 1
            crc >>= 1
            if mix:
                crc ^= 0x8C
            b >>= 1
    return crc


def raw_to_milli(lsb, msb):
    raw = (msb << 8) | lsb
    if raw & 0x8000:
        raw -= 0x10000
    return int(raw * 1000 / 16)  # truncates toward zero, as the kernel does


def w1_slave(b8, byte8=None):
    """Render w1_slave exactly as the kernel does: the nine bytes as read,
    then "crc=" with the CRC the kernel COMPUTED over bytes 0-7, and YES only
    if that matches byte 8. byte8 overrides the transmitted CRC byte, to
    make a corrupted scratchpad."""
    computed = crc8(b8)
    if byte8 is None:
        byte8 = computed
    sp = list(b8) + [byte8]
    verdict = "YES" if computed == byte8 else "NO"
    h = " ".join("%02x" % b for b in sp)
    return "%s : crc=%02x %s\n%s t=%d\n" % (h, computed, verdict, h, raw_to_milli(sp[0], sp[1]))


def read(path):
    with open(path) as f:
        return f.read()


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def masters(tree):
    out = {}
    for name in sorted(os.listdir(tree)):
        if name.startswith("w1_bus_master"):
            lines = read(os.path.join(tree, name, "w1_master_slaves")).split("\n")
            out[name] = sorted(l.strip() for l in lines if l.strip().startswith("28-"))
    return {k: v for k, v in out.items() if v}


def kernel_t(tree, rom):
    return int(read(os.path.join(tree, rom, "w1_slave")).strip().split("t=")[1])


def info(tree, rom):
    d = os.path.join(tree, rom)
    out = {}
    if os.path.exists(os.path.join(d, "ext_power")):
        out["ext_power"] = read(os.path.join(d, "ext_power")).strip() == "1"
    if os.path.exists(os.path.join(d, "resolution")):
        out["resolution"] = int(read(os.path.join(d, "resolution")).strip())
    return out


def ok_step(tree, overrides=None):
    """A step where every present ROM reads OK at the kernel's t=, except
    the overrides, plus the ghost as not_present."""
    overrides = overrides or {}
    top = masters(tree)
    roms = sorted(r for rs in top.values() for r in rs)
    readings = {}
    for rom in roms:
        if rom not in overrides:
            readings[rom] = {"code": "ok", "milli_c": kernel_t(tree, rom), "attempts": 1}
    readings[GHOST] = {"code": "not_present", "attempts": 1}
    readings.update(overrides)
    infos = {rom: info(tree, rom) for rom in roms}
    return {"topology": top, "readings": readings, "info": infos}


def classify(tree, rom):
    """What a real captured w1_slave shows, by docs/ONEWIRE.md's failure
    table, decided from the bytes alone (not by our parser). A fixture step
    is static, so a retry re-reads the same bytes: a failure takes every
    attempt."""
    lines = read(os.path.join(tree, rom, "w1_slave")).splitlines()
    first = lines[0].split(":")[0].split()
    b = [int(x, 16) for x in first]
    if not lines[0].rstrip().endswith("YES"):
        return fail("crc")
    if b == [0] * 9:
        return fail("bus_low")
    if b[0] == 0x50 and b[1] == 0x05 and b[6] == 0x0C:
        return fail("power_on_reset")
    return None  # OK: ok_step takes the kernel's t=


def real_fault_capture(name, description, source):
    d = os.path.join(ROOT, name)
    steps = sorted(x for x in os.listdir(d) if x.startswith("step-"))
    out = []
    for s in steps:
        tree = os.path.join(d, s)
        overrides = {r: c for r in all_roms(tree) if (c := classify(tree, r))}
        out.append(ok_step(tree, overrides))
    expect(name, description, False, source, out, all_roms(os.path.join(d, steps[0])) + [GHOST])


def fail(code, attempts=ATTEMPTS):
    return {"code": code, "attempts": attempts}


def all_roms(tree):
    return sorted(r for rs in masters(tree).values() for r in rs)


def expect(scenario, description, synthetic, source, steps, request):
    write(
        os.path.join(ROOT, scenario, "expect.json"),
        json.dumps(
            {
                "description": description,
                "synthetic": synthetic,
                "source": source,
                "attempts": ATTEMPTS,
                "request": request,
                "steps": steps,
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
    )


def synth(name, description, change, overrides):
    """Copy rig-steady to synthetic-<name>, apply change(tree), and expect
    OK everywhere except overrides."""
    tree = os.path.join(ROOT, "synthetic-" + name)
    shutil.rmtree(tree, ignore_errors=True)
    shutil.copytree(BASE, tree, ignore=shutil.ignore_patterns("expect.json"))
    request = all_roms(BASE) + [GHOST]
    change(tree)
    expect(
        "synthetic-" + name,
        description,
        True,
        "rig-steady with one change, from the byte patterns in docs/ONEWIRE.md",
        [ok_step(tree, overrides)],
        request,
    )


def main():
    # Real captures.
    expect(
        "rig-steady",
        "All ten rig sensors on two buses, read once, as captured.",
        False,
        "captured from the rig with w1capture on 2026-10-31",
        [ok_step(BASE)],
        all_roms(BASE) + [GHOST],
    )
    seq = os.path.join(ROOT, "rig-sequence")
    steps = sorted(d for d in os.listdir(seq) if d.startswith("step-"))
    expect(
        "rig-sequence",
        "Three sweeps 5 s apart, as captured: values move by single 1/16 °C steps.",
        False,
        "captured from the rig with w1capture -steps 3 -interval 5s on 2026-10-31",
        [ok_step(os.path.join(seq, s)) for s in steps],
        all_roms(os.path.join(seq, steps[0])) + [GHOST],
    )

    real_fault_capture(
        "rig-power-on-reset-2026-11-01",
        "A real power-on reset: one probe's VDD pulled for ~1 s, five or six times. Every read after power returned gave 50 05 .. 0c (raw 0x0550, byte 6 = 0x0C), exactly the datasheet pattern.",
        "captured from the rig with deploy/capture.sh power-on-reset (w1capture -steps 15 -interval 0s) on 2026-11-01",
    )

    real_fault_capture(
        "rig-bus-low-2026-11-01",
        "A real data line held low: bus 1's data jumper (Pi GPIO to the board) pulled for ~35 s. Every read on that bus was nine 00 bytes, crc=00 YES, t=0; bus 2 was unaffected. The sensors stayed listed on the bus throughout and read normally as soon as it was plugged back, with no power-on reset.",
        "captured from the rig with deploy/capture.sh bus-low (w1capture -steps 8 -interval 5s) on 2026-11-01",
    )

    def put(rom, text):
        return lambda tree: write(os.path.join(tree, rom, "w1_slave"), text)

    synth(
        "bus-low",
        "Data line held low: all nine bytes 0x00, which the kernel passes as crc=00 YES, t=0. Captured for real as rig-bus-low-2026-11-01; kept as the minimal case.",
        put("28-000011721aa5", w1_slave([0] * 8)),
        {"28-000011721aa5": fail("bus_low")},
    )
    synth(
        "power-on-reset",
        "Power-on reset: raw 0x0550 (85 C) with byte 6 = 0x0C (datasheet; captured for real as rig-power-on-reset-2026-11-01, kept as the minimal case).",
        put("28-0000117287e6", w1_slave([0x50, 0x05, 0x4B, 0x46, 0x7F, 0xFF, 0x0C, 0x10])),
        {"28-0000117287e6": fail("power_on_reset")},
    )
    synth(
        "real-85",
        "A genuine 85.000 C conversion (byte 6 = 0x10) that persists: published, flagged suspect, after one extra read.",
        put("28-00001173588d", w1_slave([0x50, 0x05, 0x4B, 0x46, 0x7F, 0xFF, 0x10, 0x10])),
        {"28-00001173588d": {"code": "ok", "milli_c": 85000, "attempts": 2, "suspect": "85c_unverified"}},
    )
    synth(
        "crc-no",
        "The kernel's CRC check fails (crc=… NO).",
        put("28-000011721ab6", w1_slave([0x10, 0x01, 0x4B, 0x46, 0x7F, 0xFF, 0x10, 0x10], byte8=0x01)),
        {"28-000011721ab6": fail("crc")},
    )
    synth(
        "floating-high",
        "Line floating high: every byte 0xff, which fails the CRC.",
        put("28-00001173563c", w1_slave([0xFF] * 8, byte8=0xFF)),
        {"28-00001173563c": fail("crc")},
    )
    synth(
        "bad-scratchpad",
        "CRC valid but the configuration byte is impossible for a DS18B20.",
        put("28-000011737d92", w1_slave([0x10, 0x01, 0x4B, 0x46, 0x00, 0xFF, 0x10, 0x10])),
        {"28-000011737d92": fail("bad_scratchpad")},
    )
    synth(
        "out-of-range",
        "A scratchpad decoding to 126 C, above the DS18B20's range.",
        put("28-000011721d36", w1_slave([0xE0, 0x07, 0x4B, 0x46, 0x7F, 0xFF, 0x10, 0x10])),
        {"28-000011721d36": fail("out_of_range")},
    )
    synth(
        "malformed",
        "w1_slave text that isn't the kernel's two-line format.",
        put("28-00001172de60", "garbage\n"),
        {"28-00001172de60": fail("malformed")},
    )

    def unplug(tree):
        rom = "28-000011721ce9"
        shutil.rmtree(os.path.join(tree, rom))
        for m in os.listdir(tree):
            if m.startswith("w1_bus_master"):
                p = os.path.join(tree, m, "w1_master_slaves")
                lines = [l for l in read(p).split("\n") if l.strip() and l.strip() != rom]
                write(p, "\n".join(lines) + "\n")

    synth(
        "unplugged",
        "One configured sensor gone from its bus and from sysfs: not_present after one attempt, never retried.",
        unplug,
        {"28-000011721ce9": fail("not_present", 1)},
    )

    def junk(tree):
        p = os.path.join(tree, "w1_bus_master1", "w1_master_slaves")
        write(p, read(p) + "00-e00000000000\n00-a00000000000\n")
        write(os.path.join(tree, "w1_bus_master3", "w1_master_slaves"), "not found.\n")

    synth(
        "junk-and-empty-bus",
        "Fake 00-… devices from a noisy bus search, and a third bus with nothing on it ('not found.'). Both ignored.",
        junk,
        {},
    )

    def parasite(tree):
        write(os.path.join(tree, "28-0000117287e6", "ext_power"), "0\n")
        os.remove(os.path.join(tree, "28-00001171f570", "resolution"))

    synth(
        "parasite-and-no-resolution",
        "One sensor in parasite-power mode (ext_power 0) and one with no resolution file: readings unaffected, Info reports both.",
        parasite,
        {},
    )

    # Bus swap: two sweeps; between them the bus numbers swap, as after the
    # 2026-10-18 reboot. Same sensors, same values, different bus names.
    tree = os.path.join(ROOT, "synthetic-bus-swap")
    shutil.rmtree(tree, ignore_errors=True)
    s1 = os.path.join(tree, "step-001")
    s2 = os.path.join(tree, "step-002")
    shutil.copytree(BASE, s1, ignore=shutil.ignore_patterns("expect.json"))
    shutil.copytree(BASE, s2, ignore=shutil.ignore_patterns("expect.json"))
    m1 = read(os.path.join(s1, "w1_bus_master1", "w1_master_slaves"))
    m2 = read(os.path.join(s1, "w1_bus_master2", "w1_master_slaves"))
    write(os.path.join(s2, "w1_bus_master1", "w1_master_slaves"), m2)
    write(os.path.join(s2, "w1_bus_master2", "w1_master_slaves"), m1)
    expect(
        "synthetic-bus-swap",
        "Bus numbers swap between sweeps (seen after a reboot): readings follow the ROM, not the bus.",
        True,
        "rig-steady twice, with the two buses' device lists swapped in the second",
        [ok_step(s1), ok_step(s2)],
        all_roms(BASE) + [GHOST],
    )


if __name__ == "__main__":
    main()
