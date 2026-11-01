# Reading DS18B20 sensors on a Raspberry Pi — notes for tanktemp

Everything learned from a month of experiments (2026-09-25 → 2026-10-25) on a
Pi Zero 2 W: one sensor, then three on a breadboard, then ten on a PCB across
two buses. About 87,000 logged sweeps. Written so tanktemp can be built without
re-learning any of it.

The prototype's `onewire` package now lives at `internal/onewire`; the
throwaway `tankprobe` CLI is in git history (the first commit). Real data
excerpts are in `samples/` beside this file. See [Prototype](#prototype) and
[Samples](#samples).

> **Scope warning.** Every reading we took was between 12 °C and 31 °C. A hot
> water cylinder runs from ~10 °C at the cold feed to 60–65 °C at the top
> (higher during a pasteurisation cycle or a thermostat fault). Nothing here
> has been tested at those temperatures — see [Not yet verified](#not-yet-verified).

## The rules, short version

1. **Identify sensors by their 64-bit ROM ID, never by bus.** Bus numbers swap
   between reboots, and nothing in sysfs maps a bus back to its GPIO.
2. **A valid CRC doesn't mean a real reading.** `0.000` (data line held low) and
   `85.000` (power-on reset) both pass. Check the scratchpad bytes as well.
3. **85 °C can be real in a hot water tank**, so don't blanket-reject it. Use
   scratchpad byte 6 to tell a reset apart from a real reading (details below).
4. **Retry once or twice.** Every read is a fresh conversion, so transient faults
   clear on the next read. Persistent faults don't, and should be reported.
5. **Allow ~0.8 s per sensor per bus.** Buses are independent, so read them in
   parallel: 2×5 sensors take 4 s in parallel and 8 s sequentially.
6. **Only one process should read the sensors.** A second reader makes reads
   queue in the kernel and doubles their time. Stop `templog` before running
   tanktemp on the same Pi.
7. **The Pi has no real-time clock.** Wait for NTP sync before trusting
   timestamps. Publish UTC.
8. **Report a missing sensor explicitly**, rather than letting it quietly
   disappear from the output.
9. **Readings right after hardware work are wrong.** Hand warmth adds 3–6 °C
   and takes minutes to fade.
10. **Absolute accuracy is ±0.5 °C, but each sensor's offset is stable to
    ±0.02 °C.** Calibrate the offsets out and differences between sensors
    (stratification) become very precise.
11. **`common/iot` only handles on/off inputs.** Numeric, slow (4–8 s) sweeps
    need their own loop, or an extension to the engine. See
    [Fitting into common/iot](#fitting-into-commoniot).

## Hardware

### The sensor

| | DS18B20 |
|---|---|
| Range | −55 to +125 °C |
| Accuracy (datasheet) | ±0.5 °C from −10 to +85 °C; worse outside that range (±2 °C at the extremes) |
| Resolution | 9–12 bit, configurable: 0.5 / 0.25 / 0.125 / 0.0625 °C |
| Conversion time | 93.75 / 187.5 / 375 / **750 ms** (12-bit, the default, which we used) |
| Supply | 3.0–5.5 V; we used the Pi's 3.3 V |
| Current | ~1–1.5 mA while converting; µA when idle. Ten sensors are trivial for the 3.3 V rail |
| ID | 64-bit ROM: family `28` + 48-bit serial + CRC. Factory-set, read-only |

Our parts were bare TO-92s, so we never tested the stainless waterproof probes
you'd use on a cylinder. Those probes are very often clones. Two checks that
genuine Maxim parts passed in our tests:
- the ROM serial ends in `00 00`, as in `28-0000117…`
- scratchpad byte 6 = `0x10 − (byte0 & 0x0F)` (see [Scratchpad](#the-scratchpad))

`onewire.TestRealCaptures` checks the byte 6 pattern. Run it against a capture
from any new batch before trusting the power-on-reset logic.

### Wiring

```
3.3V   (pin 1)  ── VDD of every sensor
GPIO4  (pin 7)  ── DQ of bus A sensors ──[4.7kΩ]── 3.3V
GPIO17 (pin 11) ── DQ of bus B sensors ──[4.7kΩ]── 3.3V
GND    (pin 6)  ── GND of every sensor
```

- **Use powered mode**, with VDD wired to 3.3 V, as we did. Parasite power (VDD
  tied to GND) is fragile. The datasheet advises against it above +100 °C, and
  a tank fault could reach that.
- **Give every bus its own pull-up**, 4.7 kΩ from DQ to 3.3 V. We ran 5 sensors
  per bus on short wiring with zero errors in a week. For long cable runs,
  people typically drop to ~2.2 kΩ. We haven't tested long cables.
- **For a cylinder, use a linear daisy chain**: one trunk running up the tank
  with short stubs to each sensor. Avoid a star (several long cables meeting at
  one point), which causes reflections. Maxim's app note 148, *Guidelines for
  Reliable Long Line 1-Wire Networks*, is the reference.
- **Twisted pair helps.** In CAT5, use one pair for DQ+GND and another for
  VDD+GND.
- **Near the cylinder, use cable rated for heat.** Ordinary PVC is often rated
  around 70 °C. Pipework near the top of the tank and the immersion boss gets
  hotter.
- **Keep 1-Wire wiring separate from the immersion heater's 230 V wiring.**
- **Breadboards cause faults.** Both of our unplanned wiring faults happened on the breadboard
  ([Fault signatures](#fault-signatures)). After the move to a PCB we had a week
  with zero bad reads. Solder, or use screw terminals.

### Mounting on the cylinder

We haven't tested any of this. These are things to consider:
- Use a dry pocket if the cylinder has spare ones. **Never use the
  thermostat or high-limit cut-out pockets**: those are safety devices.
- Otherwise, mount on the surface under the insulation: thermal paste, a
  strap or foil tape, then re-insulate over the top. A surface reading lags the
  water and reads slightly cooler. That's consistent from day to day, so
  calibrate it out if it matters.
- Spread sensors over the height you care about. Stratification changes
  fastest around the coil and the immersion heater.
- Record each sensor's ID against its position as you fit it
  ([Identifying sensors](#identifying-sensors)).

### How fast a sensor responds

From `samples/touch-test-single-sensor.csv`: a bare TO-92 in air, pinched
between two fingers.
- The first change showed up within one read (~1 s).
- 18.8 → 27 °C took 5 s, and 31 °C was reached after 17 s, still rising.
- Releasing it: 31 → 25 °C took 17 s. Cooling in still air is slower than
  heating from contact.

A probe in a stainless sheath, mounted on a tank wall, will be slower again.
Tank temperatures change over minutes, so a 30 s interval is plenty.

## Pi configuration

### Enabling buses

In `/boot/firmware/config.txt`, add one line per bus. Loading the same overlay
more than once is supported:

```
dtoverlay=w1-gpio,gpiopin=4
dtoverlay=w1-gpio,gpiopin=17
```

- **Reboot to apply.** To test without a reboot, load a bus for the current
  session with `sudo dtoverlay w1-gpio gpiopin=17`. It's gone on the next boot.
  `sudo dtoverlay -l` lists only overlays loaded this way, not the ones from
  `config.txt`.
- **Choose free GPIOs.** Avoid pins used by SPI, I²C or UART. This Pi has
  `spi0-0cs` enabled, so we avoided GPIO 7–11. Check a pin with `pinctrl get 17`
  first: a free pin shows `ip`, an input.
- **On a Pi 5, use the `w1-gpio-pi5` overlay instead.** Untested here.
- **The kernel modules load automatically:** `wire`, `w1_gpio`, `w1_therm`.

### Bus numbering isn't stable

The buses appear as `w1_bus_master1`, `w1_bus_master2`, … in the order they
were probed. **On 2026-10-18 the two buses swapped numbers across a reboot.**
There's also no sysfs link from a master back to its GPIO:

```
/sys/devices/w1_bus_master1           ← no parent, no "device" link
/sys/devices/platform/onewire@4       ← GPIO4 (DT name is the GPIO in hex)
/sys/devices/platform/onewire@11      ← GPIO17 (0x11)
```

So a sensor's position must come from config keyed by its ROM ID. Report the
bus name as diagnostic information only.

### Permissions

| Action | Needs |
|---|---|
| Discover buses and devices, read `w1_slave` / `temperature` / `resolution` / `alarms` | nothing (all world-readable) |
| `therm_bulk_read` trigger, writing `resolution` / `alarms` / `eeprom_cmd`, `w1_master_remove` | root |
| Changing `config.txt` | root plus a reboot |

So tanktemp can run as an ordinary user. boiler-sensor's systemd hardening
(`ProtectSystem=strict` and so on) still allows reads under `/sys`.

## The sysfs interface

All devices appear in one flat directory, `/sys/bus/w1/devices/`, whatever bus
they're on:

```
28-00001171f570/   ← a DS18B20 (family 28)
…
w1_bus_master1/    ← bus masters
w1_bus_master2/
```

### Per-sensor files (`28-xxxxxxxxxxxx/`)

| File | Example | Notes |
|---|---|---|
| `w1_slave` | 2 lines, below | **Use this one.** Every read runs a new conversion (~0.8 s) and shows the raw scratchpad, the kernel's CRC verdict and `t=` in millidegrees |
| `temperature` | `17000` | Millidegrees only, also a fresh conversion. No raw bytes, so the checks below are impossible. We haven't verified how it reports CRC failures |
| `hwmon/hwmon*/temp1_input` | `17062` | The same value through hwmon (`name` = `w1_slave_temp`) |
| `resolution` | `12` | Bits. Writable by root |
| `conv_time` | `750` | ms |
| `ext_power` | `1` | 1 = powered mode, 0 = parasite. **Check this at startup** to catch wiring mistakes |
| `alarms` | `70 75` | The TL/TH alarm bytes: 2 bytes of user EEPROM (factory 0x46/0x4b) |
| `eeprom_cmd` | write-only | `save` / `restore` the config to and from EEPROM (root) |
| `id` | 8 raw bytes | The ROM ID: `28 b6 1a 72 11 00 00 24` → `28-000011721ab6` (serial byte-reversed, CRC dropped) |

### w1_slave format

```
27 01 4b 46 7f ff 09 10 72 : crc=72 YES
27 01 4b 46 7f ff 09 10 72 t=18437
```

Line 1 has the nine scratchpad bytes, the CRC the kernel computed, and its
verdict (YES/NO). Line 2 repeats the bytes and gives `t=` in millidegrees,
`raw × 1000 / 16` truncated: 0x0127 = 295 → 18437.

### The scratchpad

| Byte | Meaning | Ours |
|---|---|---|
| 0, 1 | Temperature LSB, MSB: signed 16-bit, 1/16 °C | `27 01` = 18.4375 |
| 2, 3 | TH, TL: user EEPROM | `4b 46` (factory) |
| 4 | Config `0RR11111`, where RR = resolution | `7f` = 12-bit |
| 5 | Reserved, always `ff` | `ff` |
| 6 | Reserved. Genuine parts: `0x0C` at power-up, then `0x10 − (byte0 & 0x0F)` | `09` (= 0x10 − 7) |
| 7 | Reserved, always `10` | `10` |
| 8 | CRC-8 (Dallas/Maxim, reflected poly 0x8C) of bytes 0–7 | `72` |

### Bus master files (`w1_bus_masterN/`)

| File | Ours | Notes |
|---|---|---|
| `w1_master_slaves` | IDs, one per line | `not found.` when the bus is empty |
| `w1_master_slave_count` | `5` | |
| `w1_master_timeout` | `10` | Seconds between automatic bus searches. 59,517 searches in 7.1 days ≈ one every 10.35 s |
| `w1_master_max_slave_count` | `64` | |
| `w1_master_search` | | Search count; −1 = keep searching |
| `w1_master_remove` / `w1_master_add` | | Write an ID to drop or add a device (root) |
| `therm_bulk_read` | | Only on **one** of our two masters. See [Speed](#speed) |

New sensors appear within about one search interval. A removed sensor
disappears after a few failed searches, and until then reads of it fail.

## Taking a reading correctly

### Failure modes, and how to tell them apart

| What you see | What it means | Passes kernel CRC? | How to detect |
|---|---|---|---|
| A normal value | A real reading | yes | all the checks below pass |
| `crc=… NO` | Noise or a marginal contact | no | the verdict |
| All bytes `00`, `t=0` | **Data line held low**: pull-up lost or DQ shorted to GND | **yes**: CRC of zeros is 0 | all nine bytes are zero (byte 4 can never be 0 on a real chip) |
| `t=85000`, byte 6 = `0c` | **Power-on reset**: the sensor restarted and hasn't converted yet | **yes** | raw = 0x0550 **and** byte 6 = 0x0C |
| `t=85000`, byte 6 = `10` | **A real 85.000 °C** | yes | accept it (on genuine parts) |
| All bytes `ff` | Line floating high: no device answering | no | the verdict |
| Bytes 4, 5 or 7 wrong | Garbage from a clone or corrupted data | maybe | byte 4 & 0x9F = 0x1F, byte 5 = 0xFF, byte 7 = 0x10 |
| `ENOENT` on `w1_slave` | Sensor not on any bus (unplugged, or dropped after failed searches) | n/a | file missing |

**Why 85 °C matters here.** The obvious filter, "reject exactly 85.000", is
what our room logger uses. On a hot water cylinder 85 °C is a plausible fault
condition, and exactly the kind of reading tanktemp exists to catch. The byte
6 check separates the two cases. **The `0x0C` pattern is confirmed on the
real hardware** (2026-11-01, `testdata/sysfs/rig-power-on-reset-2026-11-01`).
One probe's VDD was pulled for about a second, five or six times, during a
continuous capture. Every read after power returned was
`50 05 4b 46 7f ff 0c 10 1c : crc=1c YES`, `t=85000`, which is exactly the
datasheet's power-up scratchpad, and the CRC passes. The probe never left the
bus, and its next read after that was a normal value. The policy stays: retry
on 85.000, and if it persists, publish it with a "suspect" flag rather than
dropping it.

### The read algorithm (`onewire.SysFS.ReadChecked`)

```
for attempt in 1..N:                 # N = 3
    read w1_slave                    # blocks ~0.8 s; fresh conversion
    if ENOENT: fail "not present"    # don't retry
    parse; check verdict + bytes     # table above
    if OK: return value
return last error
```

Retries work because every read is a new conversion: a power-on reset or a
CRC glitch is gone by the next read. A data line held low or a missing sensor
fails every attempt, which is correct, because that needs a person.

### Speed

Measured 2026-10-25 with `tankprobe`, timed between `templog` sweeps:

| Method | 2 buses × 5 sensors |
|---|---|
| Sequential | 8.0 s |
| **One goroutine per bus** | **4.0 s**: buses are independent |
| Sequential, with another process reading the same bus | 10.3 s, and individual reads took 1.5–1.6 s |

Within one bus, reads are always serialised by the kernel. Faster options:

1. **Parallel per bus**, as above. No root needed; the prototype does it with `-parallel`.
2. **Bulk conversion.** Write `trigger` to `w1_bus_masterN/therm_bulk_read`
   and every sensor on that bus converts at once (one 750 ms wait), then read
   each `temperature`. Expect ~1 s for everything. Snags: it needs root; on our
   rig only one master had the file; and the one attempt (2026-09-25) failed
   with `err=-19` while a fake device was on the bus. Untested since.
3. **Lower resolution.** At 11-bit, conversion takes 375 ms, with 0.125 °C
   steps. Write `resolution` (root), then `save` to `eeprom_cmd` so it survives
   a power cycle.

With a 30 s sweep interval, the 4 s parallel sweep is enough.

### Startup checks worth doing

- Every configured ID is present on some bus; log the bus each one is on.
- Report anything unexpected. Ignore IDs not starting `28-`: fake `00-…`
  devices appear while wiring is being changed (below).
- `ext_power` = 1 for each sensor.
- `resolution` = 12, or whatever you chose. On 2026-10-18 the kernel logged
  `resolution may be corrupted. err=-5` for 3 sensors at boot, but all were
  fine when checked.
- The clock is synced (below).

## Identifying sensors

The ROM ID is the only stable identity. To map IDs to tank positions:
- **Before fitting:** wire them up on the bench, warm each sensor in turn by
  hand (~8 s), and note which ID rises. One sensor rises at a time, and the
  others don't move: `samples/touch-test-identify-three.csv`. Label each
  probe's cable with the last 4 hex digits.
- **Or connect them one at a time** and watch which ID appears in
  `w1_master_slaves`.
- **Keep the mapping in config.** A replacement sensor has a new ID, so it's a
  config change. tanktemp should report any unknown ID it finds, so a swapped
  sensor gets noticed.

The TH/TL alarm bytes are 2 bytes of user EEPROM, so you could store a position
number in the sensor itself. It needs root to write, and we haven't tried it.

## Calibration

Factory accuracy is ±0.5 °C, but each sensor's error is very stable. Ten
sensors side by side on one PCB, as offsets from their mean, one value per day:

| | a1 | a2 | a3 | a4 | a5 | b1 | b2 | b3 | b4 | b5 |
|---|---|---|---|---|---|---|---|---|---|---|
| Mon 19 Oct | −0.18 | +0.18 | +0.07 | +0.06 | −0.08 | +0.20 | −0.11 | +0.11 | −0.10 | −0.15 |
| Sun 25 Oct | −0.16 | +0.17 | +0.07 | +0.05 | −0.08 | +0.19 | −0.10 | +0.11 | −0.10 | −0.15 |

The spread across all ten stayed at 0.38–0.50 °C all week. Within a day,
offsets varied by about 0.02 °C.

**Recommendation:** before fitting, put all the probes together in a stirred
water bath and record per-ID offsets at two temperatures that span the tank's
range (for example ~20 °C and ~60 °C). Store the offsets in config. Our
offsets were all measured at 12–18 °C, and we don't know how they change at
60 °C. Once calibrated, temperature differences between positions on the tank
can be resolved to roughly the 0.0625 °C step size.

## Time

- **The Pi Zero has no battery-backed clock.** At boot it uses the last saved
  time until NTP syncs. On 2026-10-08 our logger wrote an out-of-order
  timestamp (10:49:00 after 10:49:30): `samples/reboot-clock-not-synced.csv`.
  Wait for `/run/systemd/timesync/synchronized`, with a timeout so an offline
  boot still logs, or flag readings taken before sync.
- **Publish UTC (RFC 3339)**, as boiler-sensor does. Our CSV used local times
  with UTC offsets, which survived the BST→GMT change cleanly: 01:00–01:59
  appears twice, as `+01:00` and then `+00:00`. See `samples/dst-changeover.csv`.
- Sweeps drift by about a second (2% of rows stamped `:01`). Use the actual
  read time, not the scheduled one.

## Fault signatures

Both kinds seen for real. Sample files show them row by row.

| Symptom | Cause | Example |
|---|---|---|
| All sensors fail for one read, then one reads 85.000 | **3.3 V lost briefly**, so the sensors reset | `fault-power-loss-3v3.csv`: the jumper wobbled when the Pi was lifted (2026-09-28 11:05) |
| 0.000 on all, then all missing, back after minutes with **no** 85 | **Data line or pull-up contact.** The sensors kept power | `fault-data-line.csv` (10 min, 2026-09-28 16:53); `fault-rewire-to-pcb.csv` (2026-10-18 10:42) |
| Fake `00-…` devices, `w1_search: max_slave_count 64 reached` in dmesg | Bus search reading noise from half-made connections | during every rewiring session. Harmless; they disappear on their own |
| One sensor fails once, then recovers | A transient glitch (the old logger didn't record which kind) | once in 3 weeks (2026-09-30 12:55:30) |
| Unexplained reboot | Unknown; no persistent journal | 2026-10-08 10:48. Consider enabling a persistent journal |

A fault on one bus shouldn't affect the other: they're electrically
independent apart from shared 3.3 V and GND.

## Things that aren't faults but look like data

- **Hand warmth.** +3–6 °C within seconds, fading over 3–5 minutes. Every
  hardware intervention shows it (`fault-rewire-to-pcb.csv`: sensors came back
  about 4 °C warm). Mark or suppress readings during maintenance.
- **Direct sun.** It shows up as the sensors **diverging** from each other
  (0.9 °C spread, normal order reversed, `direct-sun-divergence.csv`), because
  air heating warms them evenly and sun doesn't. Unlikely in an airing
  cupboard, but the same applies to any local heat source near one sensor.
- **Settling after a move.** Offsets shifted for about a day after the board
  was moved (2026-09-29).
- **A heat source touching one sensor.** On 2026-10-26 at 15:57, b3 started
  reading warm because it was touching another (warm) circuit board. It rose
  +1 °C in the first 30 s, levelled off at **+4.9 °C after ~10 min**, and stayed
  there for 41 h, tracking the room's 14–21 °C swings with a constant offset.
  Its neighbours b1 and b2 also rose 0.4–0.6 °C. **Every reading passed every
  check**: CRC, scratchpad pattern, range. Only comparing b3 with its
  neighbours revealed it (`external-heat-on-one-sensor.csv`).

  On a cylinder, the same signature (one sensor stepping away from the
  expected profile and staying there) could mean a probe touching a hot pipe,
  the immersion boss or the coil flow, or a probe that has slipped out of its
  pocket. Worth detecting: compare each sensor's offset from its neighbours
  with its calibrated value, and flag a sustained change of more than about
  1 °C. Expect some legitimate change, though: a stratified tank's profile
  isn't flat the way room air is.
- **A self-heating sensor would look the same.** A damaged sensor leaking
  ~8 mA would warm itself by about 5 °C. To tell it apart from an external
  heat source, swap the sensor into another position: if the offset follows
  the sensor, the sensor is faulty.

## Resource use

- **CPU.** The shell logger used 1 h 28 min of CPU over 23 days (about 0.3%),
  mostly forking processes. A Go loop reading sysfs will be far lower.
- **Disk.** About 250 KB a day for 10 sensors as CSV at 30 s. Irrelevant if
  tanktemp only publishes to MQTT.
- **Memory.** `tankprobe` is a 2 MB static binary.

## Prototype

```
onewire/           package (now internal/onewire): parse + check w1_slave, discover buses, read with retries
  scratchpad.go    Scratchpad, CRC8, ParseW1Slave, Check() → typed errors
  sysfs.go         SysFS{fs.FS}: Buses, Thermometers, Read, ReadChecked, ReadAll
  onewire_test.go  table tests; fixtures include the real captures in testdata/
cmd/tankprobe/     CLI: one JSON line per sweep (removed; see the first commit)
sites.example.conf ID → name, height
samples/           real data (below)
```

```sh
go test -race -cover ./...                                   # no hardware needed
GOOS=linux GOARCH=arm64 go build -o tankprobe ./cmd/tankprobe # Zero 2 W on 64-bit OS
./tankprobe -once -parallel -sites sites.example.conf
```

Build for the Pi's OS: this Zero 2 W runs 64-bit (`uname -m` → `aarch64`).
boiler-sensor targets 32-bit `GOARM=6`.

Design choices, following boiler-sensor's conventions:
- **Hardware behind an interface.** `SysFS` takes an `fs.FS`, so tests use
  `fstest.MapFS` and a sequence FS that simulates a sensor recovering from a
  reset. No build tags or root needed, and CI runs on any platform.
- **Typed errors** (`ErrBusLow`, `ErrPowerOnReset`, `ErrNotPresent`, …) so the
  service can count and report each fault mode separately.
- **UTC RFC 3339** timestamps; JSON `temp_c` is `null` on failure, never 0.
- Stdlib only.

Not included: MQTT, heartbeats, status page, calibration offsets, systemd unit.
See [Fitting into common/iot](#fitting-into-commoniot): the shared engine
doesn't handle numeric sensors yet.

Example output, one sweep (`samples/tankprobe-sample-output.json`, trimmed):

```json
{"timestamp":"2026-10-25T21:06:26Z","duration_ms":3988,
 "sites":[
  {"id":"28-0000117287e6","name":"top","height_cm":150,"bus":"w1_bus_master2","temp_c":17,"attempts":1,"took_ms":803},
  {"id":"28-0000deadbeef","name":"ghost","temp_c":null,"attempts":1,"took_ms":0,"error":"28-0000deadbeef: device not present"}],
 "unexpected":["28-000011721aa5","28-000011721d36"]}
```

Verified on the real rig (2026-10-25): all 10 sensors read first time, the
missing "ghost" sensor was reported, unconfigured sensors were listed as
unexpected, and timing matched the [Speed](#speed) table.

## Fitting into common/iot

boiler-sensor and doorbell-sensor are thin wrappers around the shared engine
in `github.com/sweeney/common/iot` (v0.1.0). **That engine only handles on/off
inputs.** Its model is built around GPIO lines that change state:

| common/iot piece | What it assumes | tanktemp's reality |
|---|---|---|
| `profile.Channel` | One GPIO line: `Pin`, `ActiveLow`, `Pull`, plus `OnEvent`/`OffEvent` names | One sensor, identified by ROM ID (no pin, since several share a bus), producing a number |
| `gpio.ChannelReader` | `ReadChannels() ([]bool, error)`, read through `/dev/gpiochip0` | Temperatures in millidegrees read from sysfs, any of which can individually fail |
| `logic.Detector` | `ProcessValues(map[string]bool, …)` debounces and emits an event on each OFF→ON / ON→OFF transition | Wants periodic readings; there are no transitions to detect |
| `mqtt.FormatEventPayload` | `states map[string]logic.State`, where each is `ON`/`OFF` | `temp_c` per site, `null` on failure |
| `iot.Run` loop | Calls the reader on every tick (`--poll 100ms`) inside the same `select` that handles SIGTERM, so reads are assumed instant | A sweep blocks for **4 s** (parallel) or **8 s** (sequential). Shutdown would wait behind a read, and boiler-sensor's unit only gives it `TimeoutStopSec=10` |

The parts that aren't on/off-specific can be reused as patterns, but most of
them still take a `SensorProfile` (`mqtt.NewRealPublisherForProfile`,
`status.NewTrackerForProfile`). `mqtt.FormatSystemPayload` (for
STARTUP/HEARTBEAT/SHUTDOWN) is the main piece usable as it stands.

**Options:**

1. **Give tanktemp its own loop** (recommended to start with). Copy the shape of
   `iot.Run`: STARTUP, then a ready heartbeat once a first complete sweep is in,
   scheduled heartbeats, SHUTDOWN with a reason, and an LWT. Do the sweep in a
   goroutine so signals are handled immediately, and cancel it on shutdown.
   Keep tanktemp's own vocabulary in `cmd/tanktemp`, as CLAUDE.md requires.
2. **Extend common/iot for numeric sensors** (a v0.2.0): add a reader
   interface for numbers, such as
   `ReadValues(ctx) (map[string]Reading, error)`, and a periodic-sample loop
   alongside the edge detector. The boiler and doorbell wire-contract golden
   tests must stay byte-identical. This is worth doing once a second numeric
   sensor exists to show what the general form is.
3. **Hybrid: derive on/off channels from thresholds**, such as
   `TOP_HOT` (top ≥ 60 °C) or `TANK_DEPLETED` (middle < 40 °C), and feed those to
   the existing detector. That gets debounced events and the existing payload
   for free. The raw readings would still need their own publishing path, so
   this complements option 1 or 2 rather than replacing either.

**Related open decision:** REFACTOR.md in boiler-sensor notes that
`internal/web` is byte-identical between boiler and doorbell, and says to
decide whether it moves into `common/iot/web` "before a third sensor appears".
tanktemp is that third sensor.

## Samples

All from this rig. CSVs are `time,<one column per sensor>`, with a blank
field for a failed read (the new logger) or a literal `0.000`/`85.000` (the
old logger, before filtering).

| File | Shows |
|---|---|
| `w1_slave-raw.txt` | Raw `w1_slave` output from all 10 sensors (also the test fixture) |
| `ten-sensors-steady-night.csv` | 1 hour of quiet night: offsets and noise floor (single 0.0625 °C steps) |
| `ten-sensors-evening-heating.csv` | 4 hours of a heating cycle, rising and falling ~5 °C |
| `touch-test-single-sensor.csv` | Response time: one sensor held, then released |
| `touch-test-identify-three.csv` | Identifying IDs by warming one sensor at a time |
| `fault-power-loss-3v3.csv` | 3.3 V dropout → missed read → 85.000 |
| `fault-data-line.csv` | Data-line dropout → 0.000s → missing → recovery with no reset |
| `fault-rewire-to-pcb.csv` | Deliberate disconnect, then recovery with hand warmth |
| `reboot-clock-not-synced.csv` | Out-of-order timestamp after a reboot |
| `dst-changeover.csv` | BST→GMT, the repeated hour with distinct offsets |
| `direct-sun-divergence.csv` | Sensors drifting apart in direct sun |
| `external-heat-on-one-sensor.csv` | b3 touching a warm board: smooth rise to +4.9 °C, all checks passing |
| `tankprobe-sample-output.json` | Prototype output from the real rig |

## Not yet verified

- **Anything above 31 °C.** Accuracy, offsets and stability at 50–70 °C are
  unknown, and the datasheet's ±0.5 °C only holds up to 85 °C.
- **The power-on-reset scratchpad (byte 6 = 0x0C)** was verified on
  2026-11-01 (see [Taking a reading correctly](#taking-a-reading-correctly)).
  On 2026-10-31 a capture of all ten rig sensors
  (`internal/onewire/testdata/sysfs/rig-steady`) confirmed the
  `0x10 − (byte0 & 0x0F)` pattern again. It also showed that **a real reading
  can have byte 6 = 0x0C**, whenever the LSB's low nibble is 4 (for example
  `04 01 … 0c 10` = 16.25 °C). So byte 6 alone can't identify a reset: the
  check must also require raw = 0x0550, as `Scratchpad.Check` does.
- **Waterproof probes and clones.** Check any new batch against
  `TestRealCaptures` and the ROM pattern.
- **Long cables, daisy-chain topology, and lower pull-up values.**
- **`therm_bulk_read`**, and why it appears on only one master.
- **How the `temperature` file reports CRC failures.**
- **The cause of the 2026-10-08 reboot.**

## Related

- `scratch/temp-pi/NOTES.md`: the operational notes for the test rig itself
  (files, commands, history).
- `scratch/temp-pi/templog/`: the shell logger, with tests.
- `scratch/temp-pi/temp-sudo/`: grants temporary sudo on the Pi, with a
  timer that survives reboots.
