# Hot-water meter: plan

Draft, 2026-11-13; updated 2026-11-20 after the meter was fitted and one
night of raw edges was recorded (§0). Covers how **tanktemp** publishes a
pulsed water meter and how **washhouse** uses it. The worked example is in
[`WATER-METER-WALKTHROUGH.md`](WATER-METER-WALKTHROUGH.md), and the
interactive one, including a replay of the real night, in
[`water-meter-explainer.html`](water-meter-explainer.html).

**Status, 2026-11-20: tanktemp's part (§4) is built and live** on the rig
(sweeney/tanktemp#9). How it works now is in [`METER.md`](METER.md), the
config in the README, the wire in WIRE.md. This plan stays as the record of
why; where the build went another way:

- **Config** (§4.2): `chip` is a name (default `gpiochip0`), not found by
  label; in place of `active_low`, `bias` (`pull-up`, `pull-down`, `none`)
  and `closed_level` (`low`, `high`); `hold` was added, and holds an
  opening as well as a closing; `debounce` is 5–100 ms, `hold` at most
  100 ms, `min_interval` at most 700 ms, and `litres_per_pulse` at most
  1000. GPIO 0 and 1 are refused.
- **Unit** (§3, §4.1): `DevicePolicy=closed` with `DeviceAllow=char-gpiochip
  rw` and `SupplementaryGroups=gpio`; the `tanktemp` user is **not** added
  to `gpio`. `setup-pi.sh` and `deploy.sh` refuse a Pi with no `gpio` group;
  they don't check the line is free (a busy line is `gpio_request_failed`
  at start).
- **Wire** (§4.4): WIRE.md added `seq`, `meters: []` with none, and the
  `lost` bound of 2 × `lost`.
- **Not built yet:** the cadence flow trigger (`sweep.fast.on_flow`, a
  separate PR), the bucket test (§8), and the status page's meter view.

washhouse itself is still a plan.

**Since 2026-11-20 this plan lives in tanktemp's repo**, with tanktemp as the
single writer: washhouse sends changes to §6–7 as patches. Where it and
[`WIRE.md`](WIRE.md) differ, **WIRE.md wins**: it is the normative contract
for `water/hot/tanktemp/meter`, and §4.4 below is the draft it grew from.

Each fact is labelled the same way as the hot-water briefing:

- **label**: printed on the box, dial or spec sheet;
- **owner said**;
- **inferred**;
- **to check**;
- **measured**: seen in the first night's recording (§0).

---

## 0. Fitted: what the first real night showed

The meter was fitted on 19 Nov 2026 and wired to GPIO 27 on tanktemp's Pi
Zero W, using the Pi's internal pull-up only. The tanktemp session recorded
every raw edge with `gpiomon` (no debounce) from 17:29:48 on 19 Nov to
09:08:45 on 20 Nov UTC, alongside the probes and boiler events. The `.edges`
file is committed as test data, `internal/meter/testdata/` (owner said, 20
Nov), and `internal/meter`'s tests replay it. A `truth.json` of windows,
noise clusters and boiler events, and a scrubbed MQTT slice, stay in
tanktemp's scratch area, `scratchpad/watermeter/fixture/`.

| Finding | Detail | |
|---|---|---|
| **Reed switch** | A dry contact, closed for about **23 % of each turn** (0.40 s of 1.84 s in the bath; 3.1 s of 12.8 s on a slow tap). | measured |
| **No mechanical bounce** | Every real close and open is a single clean edge. The shortest real closed time was 343 ms. The pointer rested on the magnet for 4.4 h (18:03–22:29) with no chatter, and the next draw counted normally. | measured |
| **Electrical noise** | **15 of 277 edges, in 8 clusters, are spikes of a few µs** that return to the old level (details below). Each spike *reports the level the line already had*, because the kernel reads the pin after the interrupt, so none changes the state. | measured |
| **Real flows** | Bath 32–39 L/min (a pulse every 1.6–1.9 s; shortest interval 1.40 s); shower 7 L/min (8.5 s); sinks 4–5 L/min. | measured |
| **Overnight** | No pulse from 23:02:44 to 07:37:52. Any loop flow through the meter was well under 1 L in 8½ h. That supports "draws only", but only once the placement is confirmed (§2). | measured |
| **Lone transitions** | 18:03:29: one close, 207 s after the slow tap's last pulse. The open phase before it implies about 0.22 L/min, below Q1, so it is creep or a tap closed slowly; it counts a litre. 23:02:44: one open, which counts nothing. | measured / inferred |
| **Hot-plug kills Pis** | The previous Pi (a Zero 2 W) died when the meter leads were pushed onto the header live. **Connect only with the Pi powered off.** | owner said |

Where the noise spikes fell:

| Spikes | When |
|---|---|
| 1 | CH_ON, +0.29 s |
| 8 | HW_OFF: 6 in 247 µs, then 2 more about 0.8 s later |
| 1 | about 2 min after CH_OFF (the pump stopping after its run-on?) |
| 1 | CH_ON, +0.50 s |
| 4 | no boiler event nearby: 06:34:53, and three between 07:48 and 07:54 |

Nothing came at HW_ON, or at the 07:15 CH+HW off. The best explanation (inferred) is switching transients on the bonded earth pipework, coupling into the cable on a high-impedance pin while the Pi's 0 V floats on its USB supply.

**The replay** runs the edges through the counters below and the draw rules of §6.4. It is in the explainer, and in a scratch Python script.

| Counter | Litres | Phantom |
|---|---|---|
| Both edges, 20 ms debounce, real state changes, then the 600 ms backstop (**the plan, §4**) | **131** | 0 |
| Falling edges + 600 ms, no debounce | 140 | 9 |
| Every edge labelled "closes" | 140 | 9 |

- **Without the debounce,** the HW_OFF burst alone becomes a 2 L phantom "draw" at 76 L/min, which a flow-based kind rule would call a bath.
- **With the plan's counter,** the §6.4 rules find all 7 real draws:

  | Draw | Volume |
  |---|---|
  | bath | 35 L |
  | bath top-up | 23 L |
  | slow tap | 9 L (its 18:00:02 tail pulse is real water: closed for 3.1 s, the tap's own speed) |
  | sinks | 2, 2 and 2 L |
  | shower | 56 L at 7 L/min |

  No litre was bucket-checked yet (§8).

**What it changed in this plan:**
- §3: as wired, and the hot-plug warning;
- §4: a both-edge state machine with a mandatory debounce; `noise` and `last_closed_ms`; GPIO permissions;
- §6.2: an early flow figure from the closed time;
- §6.4: kind decided by flow first;
- §6.6: noise tied to boiler events;
- §9: updated questions.

---

## 1. The meter

| | | |
|---|---|---|
| Model | Watersavers WMP20H, K=1: ¾" BSP, 20 mm, hot-water body (red), multi-jet, screw fittings | label |
| Pulse output | **1 pulse per litre** (K=1), two-core grey cable, "supplied with DIN connector" | label |
| Rated to | T90 (90 °C), MAP16 (16 bar), ΔP63 (63 kPa pressure loss at Q4) | label (dial) |
| Flow rates | nominal Q3 4 m³/h; minimum 0.050 m³/h (**0.83 L/min**); maximum 5 m³/h (83 L/min, for 1–2 min only) | label |
| Ratio | R80 when mounted **H** (horizontal): Q1 = Q3/80 = 50 L/h, which matches the "minimum" above. **It is fitted vertically** (owner said, 20 Nov), which per the datasheet drops a class to **R40: Q1 ≈ Q3/40 = 100 L/h ≈ 1.67 L/min**, and Q2 ≈ 2.7 L/min | label + owner said + arithmetic |
| Accuracy | class 2. Under MID that means ±2 % between Q2 (1.33 L/min) and Q4 at ≤ 30 °C, **±3 % above 30 °C**, and ±5 % between Q1 and Q2 | label + MID |
| Below Q1 | "the meter will not be accurate". Vertical, expect stalls or erratic counting below about 1.7 L/min. Creep (as at 18:03:29 on the real night) is even less likely to register | label / inferred |
| Dial | five-digit m³ register, 0.1 L pointer, star-wheel leak indicator | label |
| Output type | **reed switch** (a dry contact), closed for about 23 % of each litre's turn, with no mechanical bounce (§0). Still to do: read the cable's label, and check with a multimeter that the reed is isolated from the pipe | measured / to check |

What one pulse per litre means for timing:

| Use | Flow | Gap between pulses |
|---|---|---|
| Bath, taps full on (mains pressure) | **32–39 L/min** (measured) | 1.6–1.9 s |
| Shower | **7 L/min** (measured) | 8.5 s |
| Kitchen or basin tap | **4–5 L/min** (measured) | 12–15 s |
| Dribble at Q1 (vertical, R40) | 1.67 L/min | 36 s |
| Meter's absolute maximum (Q4) | 83 L/min | 0.72 s |

**Consequences:**
- Volume resolution is 1 L.
- The latency to the second pulse is 2–25 s for real draws. A flow figure is available earlier, when the reed first *opens*, from how long it stayed closed (§6.2): about 0.5 s into a bath, 1.5 s into a shower, 3–4 s into a sink.
- Anything closer together than 0.72 s is not water.

---

## 2. Decide first: which pipe it goes on

**Fitted on the combined hot outlet**: a vertical pipe above the two tanks, just after the manifold where their hot outlets join (owner said, 20 Nov). **Where the loop's flow leg tees off relative to it is still unknown** (to check), so A or B below.

**The owner would be very surprised if the secondary loop draws from the
tanks' hot outlet pipes**, though little is known about the loop yet (owner
said, 2026-11-13). **The working assumption is therefore that the meter sees
draws only.** The two risks below stay as **things the data will confirm or
rule out**, not as design drivers.

1. **The secondary loop circulates by gravity, 24 hours a day**, out of the
   hot outlet and back into the right tank at fitting A.
   - The loss on the right tank's side is about 350–455 W.
   - At a plausible 10–20 K drop round the loop, that is about **0.3–0.6 L/min**. That's inferred: 400 W ÷ (4186 J/kg·K × ΔT).
   - If the loop's flow leg leaves *downstream* of the meter, that water passes through it all night. The rate is below the vertical Q1 (1.67 L/min), so the meter would count it erratically or not at all: several hundred phantom litres a day, about as much as the house really uses.
2. **A draw can bypass the meter.** If any tap hangs off the loop, it can be fed partly *backwards* up the return leg, out of fitting A, never passing the meter. No non-return valve is known on the loop. The result is undercounting by an unknown share.

| Placement | Circulation | Draws | Expansion (≈ 3 L each daily reheat, up to ≈ 7 L from cold) | Delivered temperature |
|---|---|---|---|---|
| **A. Combined hot outlet, before the loop's flow leg tees off** | **counted** (phantom flow; must be learnt and subtracted, §6.3) | counted, except any share fed backwards up the return | not seen | could be measured right at the meter (§9 Q5) |
| **B. Combined hot outlet, after the loop's flow leg tees off** | not seen | only the taps downstream of the tee | not seen | as A |
| **C. Cold feed to both cylinders, between the inlet group (PRV, balanced-cold take-off, check valve) and the expansion-vessel tee** | not seen | **every litre that leaves as hot** (a full cylinder takes in exactly what it gives out) | not seen (the vessel is downstream) | not available; use probe 5 or a pipe probe |
| D. Cold feed, *after* the expansion-vessel tee | not seen | all | **seen**, both ways (a multi-jet meter may count reverse flow) | – |

**Recommendation: go with the owner's placement, the combined hot outlet.**
- If the loop doesn't share that pipe, as the owner expects, the outlet is as good as the cold feed (C), and it is where the intent already is.
- washhouse **checks the assumption** instead of relying on it:
  - **Circulation would show as a slow, steady pulse train overnight with no thermal signature.** If that never appears, the question is closed. If it does, the baseline in §6.3 switches on by itself, and the meter becomes the first direct measurement of the loop.
  - **Draws bypassing the meter** would show as metered volumes persistently below the heat balance, across clean draws (§6.6).
- C stays the fallback if either check fails.

Whatever the choice:
- ~~fit it horizontally~~: it is fitted **vertically** on the combined outlet just above the tanks, after the manifold (owner said), so R40 applies (§1);
- put isolation valves either side, and the arrow with the flow;
- fit it after the system has been flushed ("do not flush through this meter").

The placement goes into config (`placement`, §5), and washhouse behaves accordingly.

---

## 3. Electrical

**As wired today (measured).**
- Red goes to header pin 13 (GPIO 27), and black to pin 14 (GND).
- The pull-up is the Pi's **internal** one only (about 50 kΩ). On BCM2835, `pinctrl` shows `--` for the pull, because the chip can't report it.
- There is no RC filter.

That high-impedance input is what picked up the spikes in §0. The meter's copper pipe is earth-bonded to the boiler's primary pipework, and the cable runs alongside bonded copper.

**Input board (out of scope here; another agent owns it).** Design for today's hardware: internal pull-up, no RC. The tanktemp session has specified a small conditioning board. The spec is in tanktemp's scratch area, `meter-interface-spec.md`.

```
 3.3V ──[10 kΩ]──┬───────────[10 kΩ]──┬── GPIO 27 (pin 13)   input, active low
                 │                    │
            meter cable            [100 nF]
                 │                    │
 GND (pin 14) ───┴────────────────────┴──
```

- **A stiff 10 kΩ pull-up** against the internal 50 kΩ.
- **10 kΩ series and 100 nF give a 1 ms RC filter.** It attenuates µs spikes heavily, and a reed closed for ≥ 343 ms passes untouched.
- These are standard values. The noise is measured; that this board cures it is still to be shown. Once it's fitted, `noise` (§4.4) should fall to zero.

**Whatever is fitted:**
- **Connect or disconnect the meter leads only with the Pi powered off** (owner said: a Zero 2 W died when they were pushed on live).
- Software must not rely on the board. The kernel debounce in §4 is what makes counting correct on today's wiring.
- Check that the reed is isolated from the pipe (multimeter, to check). If it isn't, the "dry contact" is tied to bonded earth.

**GPIO and permissions (measured on the rig).**
- GPIO 27 is free. 1-Wire uses 4 and 17, and SPI0 uses 7–11.
- `/dev/gpiochip0` is `root:gpio 0660`, and the `tanktemp` user is **not** in `gpio`.
- `tanktemp.service` has **`PrivateDevices=true`**, which hides `/dev/gpiochip*` even from members of `gpio`.
- So the unit needs either `PrivateDevices` dropped, or `DeviceAllow=/dev/gpiochip0 rw` plus `SupplementaryGroups=gpio`. That's a hardening trade-off for the owner (§9). The alternative is a separate small meter daemon (§4.1).

## 4. tanktemp: publishing the meter

### 4.1 Scope

tanktemp publishes **raw facts**: a counter, and when the last pulse came. It does not decide flow, draws or circulation. That keeps the line WIRE.md already draws: "halfway to draw detection … belongs downstream". The meter is one more sensor on the same Pi, next to the probes, so tanktemp is the right home.

A third process on the Pi, such as a common/iot-based `flow-sensor` like boiler-sensor, was considered.
- That engine emits ON/OFF transition events by polling at 100 ms. It has no counter and no numeric payload.
- It would be a third daemon with its own status topic, for one GPIO line.
- **Its one real advantage:** tanktemp's unit could keep `PrivateDevices=true` (§3).

**Decided (owner, 20 Nov): inside tanktemp.** The unit trades `PrivateDevices=true` for `DeviceAllow=/dev/gpiochip0` and the `gpio` group.

### 4.2 Config (local YAML, like the probes' ROMs)

```yaml
meters:
  - device_id: hot_water_meter     # key in devices_<site>; unique across tanks and meters
    gpio: 27                       # BCM line number; the chip is found by label, not "gpiochip0"
    active_low: true               # reed to GND with a pull-up
    litres_per_pulse: 1            # K=1
    debounce: 20ms                 # kernel debounce; MANDATORY: kills the µs spikes seen on the rig (§0)
    min_interval: 600ms            # backstop. Q4 is a pulse every 720 ms; never raise to 1 s (a real bath gives 1.4 s)
```

- The block is optional; without it, nothing changes.
- Validation:
  - device ids are unique across `tanks` and `meters`, because they share a topic namespace;
  - `litres_per_pulse` is > 0;
  - `debounce` is > 0 (the config **refuses 0**: without it, boiler switching counts phantom litres, §0) and < `min_interval`.

### 4.3 Packages (CLAUDE.md rules apply: TDD, isolated core, hand-written fakes)

| Package | Role |
|---|---|
| `internal/meter` (**core**, add to `boundaries_test.go`) | A **state machine over both edges**. `Counter.Observe(edge{mono time, level})` tracks open and closed: open then closed is one litre, subject to the `min_interval` backstop. An edge that reports the level the counter already holds is `noise`, and changes nothing. **Edge labels are never trusted on their own.** The initial level is an input, read when the line is requested, so tests can set it. It also records how long each closed phase lasted. `Snapshot()` returns count, `counting_since`, last pulse, last interval, last closed time, noise, glitches and lost. Pure and table-tested, including a replay of the first-night `.edges` fixture (131 litres, 0 phantom). |
| `internal/gpioedge` (boundary) | The **only** code that imports `go-gpiocdev`. It requests the line for **both edges**, with `WithDebounce` (gpiolib-cdev only reports a level that held for the whole period) and an event handler. It reads the initial level, then forwards `{mono time, level, line seqno}` to a `meter.Source` channel. A gap in `LineSeqno` means the kernel's event buffer overflowed; that is counted as `lost` (expected to stay 0). The kernel debounce on BCM2835 (kernel 6.18) is **not yet proven** against the spikes, so the bucket test proves it (§8). |
| `internal/meter/metertest` | `Fake` source, plus a `Contract` that every `meter.Source` must pass, like `readingtest`. |
| `internal/payload` | A `Meter` payload, with goldens. |
| `internal/cadence` | **Separate PR.** Treat a counted pulse as a trigger, so probe sweeps go fast from the first litre instead of waiting for a probe to move ≥ 1.5 °C/min. This is `sweep.fast.on_flow: true`. |
| `cmd/tanktemp` | Starts the edge source alongside `lifecycle.Run`, on its own goroutine. **A sweep blocks for 4–8 s, and pulses must never wait for it.** Publishes on each pulse and with each sweep (§4.5). Publishes a final meter state before SHUTDOWN. |
| `cmd/pulsewatch` | Rig tool, like `w1capture`. Prints each edge: monotonic time, level, interval, and how long the contact stayed closed (from the state machine, not the labels). It also says whether the edge counted, was noise, or was a glitch. It can run raw, with no debounce, to measure noise. Used for the bucket test (§8) and to tune `debounce` and `min_interval`. |

**Time.**
- Intervals come from kernel event timestamps, which are monotonic. NTP stepping the wall clock at boot can't fake or hide a pulse; this is the same reasoning as `cadence`.
- `last_pulse_at` is converted to the wall clock at publish time: `wall_now − (mono_now − mono_event)`, to the millisecond.

**No persistence.** The count starts at 0 when tanktemp starts, and `counting_since` says when. Consumers take differences and treat a new `counting_since` as a reset with a gap. Nothing is written to the SD card. A register that tracks the dial is an option for later (§9 Q6).

### 4.4 Wire: the new topic

Like every change to tanktemp's wire, this one starts in `docs/WIRE.md`, with goldens. **WIRE.md's Meter section supersedes this draft.** Where they differ: a per-run `seq`, with de-duplication on (`counting_since`, `seq`); `meters` is `[]`, not `null`, with no meters; and with `lost` > 0 the count is a lower bound, short by at most `lost` + 1 per loss.

| Topic | When | QoS | Retained |
|---|---|---|---|
| `{prefix}/meter`, i.e. `water/hot/tanktemp/meter` | each counted pulse, and with every sweep | 0 | yes |

Topics were decided on 20 Nov 2026. tanktemp's prefix moves from `energy/hot_water` to `water/hot/tanktemp`, and washhouse owns `water/hot/{state,tank/{id},events,status}`. The meter topic is one per tanktemp, not one per device: the `device_id` is in the payload, and a second meter would become `meter/{id}`. It does **not** match `{prefix}/+/state`. statehouse's tanktemp adapter subscribes to exactly `+/state` and `status` (`adapter.go`), so it ignores the new topic. greenhouse doesn't subscribe to tanktemp's topics at all.

```json
{
  "device_id": "hot_water_meter",
  "timestamp": "2026-11-20T07:12:57Z",
  "clock_synced": true,
  "trigger": "open",
  "litres_per_pulse": 1,
  "level": "open",
  "counting_since": "2026-11-19T22:41:07Z",
  "count": 1833,
  "last_pulse_at": "2026-11-20T07:12:57.312Z",
  "last_interval_ms": 6699,
  "last_closed_ms": 1541,
  "noise": 0,
  "short": 0,
  "glitches": 0,
  "lost": 0
}
```

| Field | Meaning |
|---|---|
| `timestamp` | To the second, as everywhere in tanktemp. It is the pulse time for `trigger: pulse`, and the **sweep's** start for `trigger: sweep`, so a sweep's tank states and meter count are a matched snapshot. |
| `trigger` | `pulse` (the counted close, published immediately), `open` (the open that ends that pulse, carrying a fresh `last_closed_ms`), or `sweep`. |
| `counting_since` | When this count started (tanktemp's start). A change means a reset, and pulses between the two runs were not seen. |
| `count` | Counted pulses since `counting_since`. Litres = `count × litres_per_pulse`. Exact, apart from `lost`. |
| `last_pulse_at` | Milliseconds. `null` before the first pulse. |
| `last_interval_ms` | Monotonic time between the last two counted pulses. `null` before the second. |
| `last_closed_ms` | How long the reed stayed closed in the last completed pulse. It works as a speedometer: flow ≈ 60 × duty ÷ closed seconds, with a duty of about 0.23 (§6.2). A raw fact; tanktemp doesn't interpret it. `null` until a pulse has closed and opened. |
| `level` | The current debounced state, `open` or `closed`. Closed for hours means the pointer is resting on the magnet. Never unknown: a request that can't read the level fails with `gpio_request_failed`. |
| `noise` | Edges that got through the debounce but didn't change the level. With the kernel debounce working this should stay 0, because the spikes in §0 never reach user space. A rise means the debounce isn't holding. |
| `short` | Closings that didn't stay closed for the user-space hold (belt and braces behind the kernel debounce). Expected 0. |
| `glitches` | Real state changes ignored as closer than `min_interval`, the backstop. Expected 0. |
| `lost` | Edges the kernel dropped (a `LineSeqno` gap). |

**Replay rule.** A pulse message and its open message can share `counting_since`, `count` and `timestamp`, so a consumer de-duplicates on `(counting_since, count, trigger, timestamp)`. Or on a per-run `seq`, if tanktemp adds one (to agree in WIRE.md).

**Volume.**
- Pulse and open messages: two per litre, so roughly 400–1,200 a day of real use.
- Sweep messages: 2,880 a day at 30 s, the same as one tank's states.

**Status, additive.**
- `meters: ["hot_water_meter"]` (`null` without the block), next to `tanks`.
- A new `startup_problems` code, `gpio_request_failed` (line busy, no permission, chip not found).
- statehouse decodes status with a lenient `json.Unmarshal`, so the new key is safe.

**WIRE.md "Changes" entry:** "new topic `{prefix}/meter`; status gains `meters`; state goldens unchanged."

### 4.5 Publishing path

```
GPIO edges ─► gpioedge (both edges, kernel debounce, mono ts, level) ─► meter.Counter ─┬─► publish {…/meter, trigger: pulse}   (immediately)
                                                                   ├─► cadence: flow trigger → fast sweeps   (separate PR)
lifecycle sweep ─► tank states + meter.Snapshot() ─────────────────┴─► publish {…/meter, trigger: sweep}   (same timestamp as the sweep)
```

Check that `mqttpub.Publish` is safe to call from the edge goroutine while a sweep is publishing. paho is safe; the wrapper needs confirming.

### 4.6 Health

- `/healthz` and `/api/status` gain a `meters` block: pin, level, count, `counting_since`, last pulse, noise, glitches, lost.
- A failed GPIO request makes tanktemp `degraded`, but probes still publish.
- **A silent meter is not a fault.** Days without pulses are possible, and only washhouse can tell (§6.6).

### 4.7 Tests, and the rig

- **Red/green tests** cover:
  - the `Counter` table: state changes, same-level noise, glitch rule, seqno gaps, initial level;
  - a replay of the first-night `.edges` fixture through a simulated debounce: 131 litres, 0 phantom, and the noise clusters rejected;
  - payload goldens;
  - config validation;
  - a `cmd` scenario test with `metertest.Fake`, covering pulse during a sweep, SHUTDOWN, and reconnect.
- **`setup-pi.sh --check`** additionally reports:
  - whether the service user is in `gpio`;
  - whether the configured line is free (`pinctrl get`);
  - whether the unit can see `/dev/gpiochip0` (`PrivateDevices`, §3).

  Without `--check`, it adds the user to the group, idempotently, behind the usual `--yes` prompt.
- **Deploy** with `./deploy.sh --check` first, as always.

---

## 5. devices_home entry

The owner writes this; the JSON shape is drafted here. `common/devices` ignores keys it doesn't know, so a `meter` block is safe today. Decoding it properly would be a `devices` v0.3.0, which is only needed when something reads it.

```json
"hot_water_meter": {
  "name": "Hot water meter",
  "class": "flow_meter",
  "room": "<the cylinders' room>",
  "telemetry": true,
  "scheme": "tanktemp",
  "meter": {
    "litres_per_pulse": 1,
    "medium": "hot_water",
    "placement": "hot_outlet_before_loop",
    "q1_lpm": 1.67,
    "mounting": "vertical",
    "accuracy_class": 2,
    "serves": ["hot_water_cylinder_left", "hot_water_cylinder_right"]
  }
}
```

- `placement` is one of `hot_outlet_before_loop` (A), `hot_outlet_after_loop` (B), `cold_feed` (C).
- `flow_meter` must be a class that is **not** occupancy-relevant in `statehouse_classes`. washhouse's draw events carry the occupancy evidence; the raw meter doesn't.

---

## 6. washhouse: using the meter

**Principle: the meter measures, and the thermal model explains.** With a healthy meter, draws are found and sized by pulses. The probes then say which tank gave the water, how hot it was, and whether the estimate of hot water available is right. Without the meter, washhouse falls back to the thermal detector already planned. **Both paths always exist**, and every draw says which one produced it.

### 6.1 Ingest

- `wire/tanktemp` (the common wire module from the main plan) gains a `Meter` type and decoder.
- washhouse subscribes to `water/hot/tanktemp/meter` (tanktemp's prefix + `/meter`).
- It applies the same freshness rules as tank states: stale after 90 s (3 × the slow interval), `clock_synced` must be true, and de-duplication uses the replay key (§4.4).
- **Counting rules:**
  - If `count` jumps by more than 1 between messages (a lost QoS 0 message), the missing pulse times are spread evenly between the two known ones.
  - A new `counting_since` closes the old series. The gap goes into the meter's state as `unseen`, and draws in that gap fall back to thermal.

### 6.2 Flow

From the pulse times:
- `flow_lpm` = 60 × `litres_per_pulse` ÷ the interval. It decays to 0 once the gap since the last pulse exceeds max(30 s, 4 × the last interval), capped at 120 s.
- **Early flow, from the closed time.**
  - The reed is closed for a fixed share of each turn: a duty of about 0.23, measured on the bath and the slow tap.
  - So at the first *open*, flow ≈ 60 × duty ÷ (`last_closed_ms` / 1000).
  - On the real night this gave a flow figure 0.5 s into a bath, against 1.8 s at the second pulse; 1.4 s into the shower, against 5.5 s; and 3–4 s into a sink, against 12–17 s. The two methods agreed within about 20 % (the closed time read lower on sinks).
  - **Learn the duty** from steady draws (median closed ÷ interval); don't hard-code it, because it depends on the magnet and the reed.
  - A closed phase far longer than the flow implies (27 s on the slow tap) means the wheel paused partway round.
- `today_l`: litres since local midnight. The timezone comes from the site config.

### 6.3 Circulation baseline (dormant unless the data shows a loop)

Not expected (§2): the owner doesn't think the loop draws from the outlet pipe. **washhouse watches for it** and switches this on only when it sees slow pulses with no thermal signature, at least 3 of them overnight across 2 nights.

- **What it looks like:** a slow, steady pulse train, every 1–5 minutes, with no thermal signature. Probe 1 doesn't fall, because no cold water enters.
- **How it's learnt:**
  - an EWMA of the flow rate over hours, using only stretches with no draw candidate and intervals over 60 s;
  - bounded to at most 1.5 × Q1 (vertical Q1 = 1.67 L/min).
- **Later:** model it against the thermosiphon head, roughly ∝ (top temperature − ambient). It should fall overnight as the tank cools.
- **Published** as `circulation_lpm`.
- **What it gives:** the loop's heat loss, measured. Loop W ≈ flow × 4186 × (outlet temperature − right P4), where right P4 reads the return water. It also gives the before/after of any loop fix as a number.

### 6.4 Draw detection, metered

| Step | Rule (starting values; every threshold is in config) |
|---|---|
| Candidate | An interval < min(36 s, baseline interval ÷ 3). That is faster than the vertical Q1 (1.67 L/min) and clearly faster than circulation. The slowest real draw seen (3.6 L/min, 16.6 s) is well inside it. |
| `draw_started` | **At the first short interval**, i.e. the second pulse. `started_at` = first pulse − ½ × that interval (the tap opened partway through a litre). `detected_at` = the second pulse. |
| Single pulse | One isolated pulse is not a draw on its own (rotor creep, the pointer resting on the reed; e.g. 18:03:29 on the real night). It counts if a thermal draw signature (P5 rise or P1 drop) arrives within 2 min. The closed time can also tell a real litre (about 3 s closed at sink flow) from creep. |
| `draw_updated` | Every 30 s while flowing, with running volume and flow. This refreshes statehouse's 30-min TTL. |
| `draw_ended` | No pulse for max(30 s, 4 × the median interval), capped at 120 s. `ended_at` = last pulse + ½ × median interval. |
| Merge | Draws less than 60 s apart are one draw (a shower turned off to lather, a sink refilled). |
| Volume | `volume_l` = pulses in the draw × `litres_per_pulse` (− baseline × duration, only if §6.3 is active). `volume_uncertainty_l` = 1 + 3 % (placement A/B) or 2 % (C). `volume_source: "metered"`. |
| Masked by reheat | **No longer loses the volume.** `masked_by_reheat` stays as a fact, but the volume is metered. |
| Kind | **Flow first.** A mean flow of 20 L/min or more is a `bath`: the real bath ran at 32–39 L/min, against 7 for the shower and 4–5 for sinks. The simulation had assumed a much slower bath. Otherwise use **hot** litres, which is what the meter sees (a mixer shower takes only about 60–70 % of its flow from the cylinder): tap < 15 L; bath ≥ 80 L; shower in between, lasting ≥ 3 min. Volume alone would have called the real 23 L bath top-up a tap. A hint, with its assumptions in the payload, and tuned from real draws. |
| Confidence | Metered, with healthy meter state: **0.95**, or 0.85 for a single pulse with thermal corroboration. Thermal fallback keeps its existing score. |

### 6.5 Energy, tank split, and calibration: the big win

- **Delivered energy.** `delivered_kwh` = Σ per litre × (T_out − T_cold) × 4.186 / 3600.
  - **T_out** is P5 *during flow*. Its lag is about 18 s, so use max(P5, upper bulk) for the first sweep. A pipe probe at the meter is better (§9 Q5).
  - **T_cold** is the mains temperature. It is learnt as P1's minimum after big draws, because it changes with the season; the default is 10 °C.
- **Split by tank.** Both tanks usually supply a draw (measured), but the meter sees them together.
  - Share = each tank's ΔE, corrected for loss and gain, ÷ the total ΔE.
  - When masked by reheat, use the learnt long-run share instead. That is an EWMA over clean draws, which is itself a measure of the hydraulic balance between the tanks.
  - Each tank's entry in `tanks[]` gets `volume_l`, `share` and `share_source` (`heat_balance`, `learnt`).
- **Calibration**, per clean draw: not masked, both tanks fresh, and the meter healthy.
  - Record `r` = the thermal model's net ΔE ÷ `delivered_kwh`, and the model's heat-balance volume against the metered volume.
  - **This is the ground truth the model never had.** It tests the guessed vessel geometry (INNER_D, DOME_H and Z0 in `avail.py`). It also tests the smearing bias the explainer's simulation found: a big shower overcounted at 84 L estimated against 63 L true.
  - **First,** expose the running median and IQR of `r` on `/api/v1/model`.
  - **Later,** fit a per-tank effective-volume factor, then the geometry, by least squares over draws.
- **Checking the estimate of hot water available.**
  - After each big draw, compare the model's fall in `usable_l40` with the metered litres converted to a 40 °C equivalent: L × (T_out − T_cold) ÷ (40 − T_cold).
  - On the rare day the outlet falls below 40 °C mid-draw, the metered litres since the last full state *are* the usable volume. That measures the 0.85 mixing factor directly.

### 6.6 Cross-checks and new events

- **`meter_suspect`**, which makes `/healthz` `degraded`, fires when any of these happen:
  - a thermal draw with an inlet drop of at least 8 °C (confidence ≥ 0.9) **and zero pulses** in its window;
  - meter messages stop while tank states keep arriving;
  - `noise`, `glitches` or `lost` climbing.

  It clears (`meter_ok`) after the next metered draw with thermal corroboration.
- **Noise attribution.** tanktemp doesn't subscribe to the boiler, by design ("raw facts"), so washhouse joins `noise` increments with boiler events and reports noise per transition type as a model statistic. A rise after the input board is fitted is a wiring fault, not water.
- **Bypass check (placement A/B).** Over clean draws, a persistent metered volume below what the heat balance implies points at draws fed backwards up the return. Report it as a model statistic; don't act on it.
- **`continuous_flow_started` / `_ended`.** Net flow above the baseline with no idle gap of 15 minutes or more for 2 hours: a hot tap left dribbling, or a leak. statehouse could route it to notifications later.

### 6.7 Outputs (additive to washhouse's v1 schemas)

- **System state:**

  ```
  meter: {state, placement, flow_lpm, draw_flow_lpm, circulation_lpm, today_l, counting_since}
  ```

  `state` is `ok | stale | suspect | absent`.
- **Draw events:**
  - `volume_l`, `volume_source` (`metered | heat_balance`), `volume_uncertainty_l`;
  - `volume_l40`: the metered litres converted to 40 °C water, L × (T_out − T_cold) ÷ (40 − T_cold), the unit the rest of washhouse uses;
  - `heat_balance_l40`, always present for comparison;
  - `flow_lpm_mean`, `flow_lpm_peak`, `delivered_kwh`;
  - per-tank `share`.
- **Influx:**
  - `washhouse_draw` gains the fields above;
  - new `washhouse_flow`: one-minute buckets of `litres`, `litres_net` and `circulation_lpm`, for daily-usage charts;
  - new `washhouse_calibration`: one point per clean draw.
  - Still nothing goes into greenhouse's climate measurement.
- **API:**
  - `/api/v1/state` includes the meter block;
  - `/api/v1/model` includes calibration;
  - `/api/v1/draws` shows both volumes.

---

## 7. statehouse

**No new work beyond the main plan.**
- Metered draws arrive at confidence 0.95, so the existing gate (≥ 0.7 and ≥ 0.5 L) passes every real draw, including the masked evening ones it used to drop.
- `draw_started` comes about as fast as before. The second litre arrives 2 s (bath) to 17 s (sink) after the tap opens. The thermal path often triggers on the first sweep (0–20 s in the simulation). The gain is certainty and size, not speed. The closed-time flow (§6.2) could start a draw at the first open, but this plan keeps the second pulse as the trigger until the duty is learnt.

Later options:
- route `continuous_flow` to notifications;
- show `draw_flow_lpm` as a reading.

---

## 8. Order of work

1. ~~Fit the meter~~ (done 19 Nov), ~~identify the output~~ (a reed), ~~pick a GPIO~~ (27). Still to do:
   - **confirm the placement** (§2): where the loop's flow leg tees off, and whether any tap hangs off the loop;
   - ~~cable length~~ (2 m); the multimeter test and the cable label are deferred.
2. ~~Fit the input board~~: out of scope here (another agent owns it). The software must count correctly without it.
3. **tanktemp, in its own repo, under its CLAUDE.md:**
   1. the WIRE.md change and goldens;
   2. `meter` and `gpioedge` with the fake and contract;
   3. config, the cmd wiring and health;
   4. `pulsewatch`;
   5. the `setup-pi.sh` check.

   The cadence flow trigger follows as a separate PR. Deploy `--check` first.
4. **The bucket test** (deferred by the owner):
   - draw 10 L into a measured bucket from a full-bore tap, three times;
   - compare `pulsewatch`, the published `count` and the dial;
   - **switch the boiler programme HW and CH on and off during the run**, to prove the kernel debounce live: `noise` and phantom litres must stay 0.

   A small read-only script reads the count from tanktemp's `/api/status` before and after (per CLAUDE.md: `--check` only, tested against a fixture). Then the same per outlet, which also gives the per-outlet flow rates the briefing lacks.
5. **Record a week** of `water/hot/#` with the meter, as the first metered fixtures. The fixture-scrubbing rules from the main plan apply.
6. **Add the devices_home entry** (§5).
7. **washhouse:**
   - `wire/tanktemp` Meter;
   - ingest;
   - flow;
   - the baseline (A only);
   - the metered detector, alongside the thermal one;
   - energy, split and calibration;
   - the new events and outputs.

   The first-night recording is the first real fixture. Synthetic pulse fixtures come from the explainer's simulator, which knows each draw's true volume. The recorded week comes next.

The washhouse parts fit the main plan's phases 2 (detection), 3 (wire) and 5 (service). Since washhouse doesn't exist yet, the metered path can be designed in from the start, not retrofitted.

---

## 9. Open questions for the owner

1. **Placement:** A, B or C (§2)? If on the hot side, where does the loop's flow leg tee off relative to the meter?
2. **Are any taps on the loop itself?** That decides whether draws can bypass the meter up the return.
3. ~~Reed or powered output?~~ A reed (§0). Still open: **is it isolated from the pipe** (multimeter), and **what does the cable label say?**
4. ~~Which GPIO~~ (27). ~~Cable run~~: 2 m.
5. **A pipe probe at the meter?** One more DS18B20 on the combined outlet would give the true delivered temperature, which beats heat-sunk P5 for `delivered_kwh`. tanktemp would need a home for a probe that isn't a tank, without statehouse's adapter reading it as a cylinder. One option is to attach it to the meter's device, publishing `temp_c` in the meter payload.
6. **A persisted register** that follows the dial (set once from a dial reading, saved at shutdown and every 10 min), or is `counting_since` + `count` enough?
7. ~~Topic naming~~. Decided: `water/hot/tanktemp/meter`.
8. ~~GPIO access for tanktemp's unit~~. Decided: inside tanktemp, with `DeviceAllow` and the gpio group (§4.1).
9. **The 07:48–07:54 spikes** had no boiler event nearby. What switched then (a pump, a fridge, the immersion)?
10. ~~Commit the first-night fixture~~. Decided: committed in tanktemp, `internal/meter/testdata/`.
