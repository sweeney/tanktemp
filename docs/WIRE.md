# tanktemp MQTT wire contract

This document is normative. The golden files in `internal/payload/testdata/`
pin these bytes exactly, and a diff there is a breaking change. New fields
are additive. Removing or renaming a field is a major change, announced here
first.

All payloads are UTF-8 JSON objects with a fixed key order. Times are UTC,
RFC 3339, to the second. **An unknown value is `null`, never `0`**, and
every key is always present (except `reason`, below).

## Topics

`{prefix}` is `mqtt.topic_prefix` in the config. The default is
`water/hot/tanktemp`; test deployments use `test/water/hot/tanktemp`. It was
`energy/hot_water` until 2026-11-20 (see Changes).

| Topic | When | QoS | Retained |
|---|---|---|---|
| `{prefix}/{device_id}/state` | every sweep, one message per tank (how often: see Cadence) | 0 | yes |
| `{prefix}/status` | STARTUP, the first HEARTBEAT (`ready`), each scheduled HEARTBEAT, a HEARTBEAT after every MQTT reconnect, SHUTDOWN; and the last will | 1 | yes |
| `{prefix}/meter` | when a water meter is configured: each counted litre, the opening that ends it, and every sweep (see Meter) | 0 | yes |

`device_id` is the tank's key in `devices_<site>`, so it is `[a-z0-9_-]+`
and safe as a topic segment. There is one tanktemp per site: a second one
would need its own prefix, because the status topic belongs to the daemon,
not to a tank. The meter topic is one per tanktemp too, with the meter's
`device_id` in the payload; a second meter would get `{prefix}/meter/{device_id}`,
announced here first. `{prefix}/+/state` doesn't match `{prefix}/meter`, so a
consumer of tank states never sees meter messages.

## State

```json
{
  "device_id": "hot-water-cylinder",
  "timestamp": "2026-11-02T21:06:26Z",
  "clock_synced": true,
  "sweep_ms": 3988,
  "sensors": [
    {"position": "bottom", "order": 1, "height_mm": null, "rom": "28-000011721ce9",
     "temp_c": 12.0625, "raw_c": 12.0625, "offset_c": 0,
     "error": null, "suspect": null},
    {"position": "top", "order": 2, "height_mm": 1500, "rom": "28-0000117287e6",
     "temp_c": 61.185, "raw_c": 61.375, "offset_c": -0.19,
     "error": null, "suspect": null}
  ]
}
```

(The real payload is one compact line; it is pretty-printed here.)

| Field | Meaning |
|---|---|
| `device_id` | the tank |
| `timestamp` | when the sweep started |
| `clock_synced` | whether NTP had synced. The Pi has no real-time clock, so `false` means the timestamp may be wrong |
| `sweep_ms` | how long the whole sweep took, all tanks |
| `sensors` | **every** configured sensor of this tank, in `order`: 1 is the bottom, so the list runs bottom to top, the way heights and probe labels count. A failing sensor is never left out |
| `position`, `order`, `height_mm`, `rom` | from the config. `height_mm` is the probe's height above the cylinder's base in mm, `null` if not configured. The ROM is the physical sensor; it changes when a probe is replaced, while the position doesn't |
| `temp_c` | calibrated °C: `raw_c + offset_c`, at most 4 decimals. `null` when the read failed |
| `raw_c` | the sensor's own value, exactly: a multiple of 0.0625 (the kernel's `t=` truncates odd sixteenths, e.g. 12.0625 to 12.062; tanktemp doesn't). `null` when the read failed |
| `offset_c` | the calibration applied, `0` when uncalibrated |
| `error` | `null`, or why there is no value: one of `not_present`, `bus_low`, `crc`, `power_on_reset`, `bad_scratchpad`, `out_of_range`, `malformed`, `read_error` |
| `suspect` | `null`, or `85c_unverified`: a value of exactly 85.000 °C that passed every check, and that one extra read didn't replace with a different good value (it read 85 again, or that read failed). It's probably real (a cylinder can reach it), but 85 °C is also the sensor's power-on value. While a probe is suspect, tanktemp's `/healthz` reports `degraded` |

The retained state means a consumer that connects late gets the last
reading. Its `timestamp` says how old it is.

## Status

```json
{
  "online": true, "event": "HEARTBEAT", "timestamp": "2026-11-02T21:15:00Z",
  "version": "4aba4d5", "ready": true, "uptime_s": 86400,
  "tanks": ["hot-water-cylinder"], "meters": ["hot_water_meter"],
  "sensors_ok": 9, "sensors_failed": 1,
  "unexpected": ["28-000011721aa5"],
  "startup_problems": [{"rom": "28-0000117287e6", "code": "parasite_power", "message": "…"}]
}
```

| Field | Meaning |
|---|---|
| `online` | `true` from tanktemp; `false` in the last will and on SHUTDOWN |
| `event` | `STARTUP`, `HEARTBEAT`, `SHUTDOWN` or `LWT` |
| `version` | the commit tanktemp was built from |
| `ready` | `true` once the first complete sweep has been taken (and handed to MQTT; it is `true` even if that publish was lost). A HEARTBEAT after a reconnect that happens before the first sweep says `false` |
| `uptime_s` | seconds since start |
| `tanks` | the configured device IDs, in config order |
| `sensors_ok`, `sensors_failed` | from the last sweep; `null` before the first |
| `unexpected` | thermometers seen on a bus but not configured (a swapped probe shows up here). `null` before the first sweep |
| `meters` | the configured meters' device IDs, in config order; `[]` when there are none |
| `startup_problems` | from the startup checks: `rom` (`null` for a bus-wide problem, and for a meter), `code` (`discover_failed`, `not_present`, `parasite_power`, `resolution`, `inspect_failed`, or for a meter `gpio_request_failed`: the line is busy, not permitted, or its chip wasn't found, so the meter isn't counting), and a human `message` naming what failed |
| `reason` | SHUTDOWN only: the signal (`SIGTERM`, `SIGINT`) or `context` |

The **last will** is exactly `{"online":false,"event":"LWT"}`, retained, QoS
1. The broker publishes it if tanktemp vanishes without a SHUTDOWN. It
carries no timestamp, because a will is fixed when tanktemp connects.

## Meter

A reed-switch water meter on a GPIO line: the meter's litre pointer
carries a magnet that closes the reed once per turn. tanktemp publishes
**raw facts only**: the count, when the last litre came, how long the reed
stayed closed, and why edges were rejected. Flow, draws, circulation and
what a rejected edge means are for consumers (washhouse).

```json
{
  "device_id": "hot_water_meter",
  "timestamp": "2026-11-20T07:12:58Z",
  "clock_synced": true,
  "trigger": "open",
  "seq": 3671,
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
| `device_id` | the meter: its key in `devices_<site>` |
| `timestamp` | to the second. For `pulse`, the counted closing; for `open`, the opening (exactly: `last_pulse_at` + `last_closed_ms`; use that, not this rounded time, for flow); for `sweep`, the sweep's start, the same as that sweep's tank states. A `sweep` message is the meter **as of the sweep's start** (taken then, not after the sweep), so it and the tank states are a matched snapshot |
| `clock_synced` | as for state |
| `trigger` | `pulse`: a litre was counted, published within `meter.hold` + 50 ms of its closing (tanktemp checks a held closing at least every 50 ms, and at every edge). `open`: the opening that ends a counted litre, with its `last_closed_ms`. `sweep`: with every sweep, so the count is never older than a sweep |
| `seq` | numbers this run's meter messages as they're published: 1, 2, 3… from `counting_since`, with no gaps (one missing means MQTT lost it). Messages are in the counter's order, so `count` never falls as `seq` rises. A retained replay repeats one; **de-duplicate on (`counting_since`, `seq`)**. Only `count` measures litres: tanktemp may skip a litre's own message when the broker is too slow (and replaces a waiting `sweep` message with a newer one), which a consumer sees as `count` jumping by more than one |
| `litres_per_pulse` | from the config (the meter's K) |
| `level` | the reed now, after debounce: `open` or `closed`. Closed for hours means the pointer is resting on the magnet. A new closing reads `closed` for up to `meter.hold` + 50 ms before its `pulse`, while it's being held; that is not a fault. Never unknown: tanktemp reads it when it requests the line, and a line it can't read is `gpio_request_failed` |
| `counting_since` | when this count started: when tanktemp requested the line. A new value is a reset; anything between the two runs went unseen |
| `count` | litres counted since `counting_since`, as pulses: litres = `count` × `litres_per_pulse`. A message lost in delivery loses nothing: the next one carries the total |
| `last_pulse_at` | the last counted closing, **to the millisecond** (unlike other times). The kernel stamps an edge once its debounce has confirmed it, so this, like every meter time, is about `meter.debounce` (20 ms) after the reed moved; intervals and closed times are unaffected. `null` before the first |
| `last_interval_ms` | between the last two counted closings, on the monotonic clock. `null` before the second |
| `last_closed_ms` | how long the reed stayed closed in the last counted litre that has ended, on the monotonic clock. The reed is closed for a roughly fixed share of each turn (about 0.23 on the rig), so this gives the flow within one litre; tanktemp doesn't interpret it. Minutes or hours means the pointer rested on the magnet (the rig's first night had 4 h 26 m): it is not a flow. `null` until one has |
| `noise` | edges reporting the level the reed already had: a spike shorter than the kernel could read (or, should the kernel ever send one, an event out of order). They change nothing. Expected 0 with the kernel debounce on; a rise means it isn't holding |
| `short` | closings that reopened within `meter.hold`: a spike that did flip the reading. Neither edge counts. Expected 0 |
| `glitches` | held closings less than `meter.min_interval` after the last counted one. Not counted. The meter's maximum flow is a litre every 0.72 s, so expected 0 |
| `lost` | edges the kernel dropped (its event buffer overflowed). After a loss tanktemp takes the line's level as it now is and counts nothing across the gap, nor a closing still being held when it came: a litre may be lost, never invented. **So with `lost` > 0, `count` is a lower bound, short by at most 2 × `lost`** (a loss of n edges can miss up to n/2 + 1 litres: those among the dropped edges, the held one, and the one in progress at the resync; and n/2 + 1 ≤ 2n). Expected 0 |

`noise`, `short`, `glitches` and `lost` are totals since `counting_since`.

**How a litre is counted**, so a consumer knows what the count means: one
litre per change from open to closed, judged on the level read after each
edge, never on the edge's label alone. The checks, in order: a gap in the
kernel's event numbers (`lost`), an edge that doesn't change the level
(`noise`), a closing that doesn't hold for `meter.hold` (`short`), and a
closing too soon after the last counted one (`glitches`). The kernel
debounces the line first (`meter.debounce`, required, at least 5 ms). On the rig's
first night, with no debounce, 15 of 277 edges were spikes that came as the
boiler's programmer switched; counting every falling edge gave 140 litres,
these rules 131.

**Volume.** One per sweep, about 2,880 a day at 30 s (the same as one tank's
states), plus two per litre (`pulse`, `open`): roughly another 400–1,200 a
day of real use.

**Delivery** is as for state: QoS 0, retained, nothing queued while the
broker is unreachable. A consumer that needs every litre's time uses
`count` to see how many it missed between two messages.

## Cadence

How often a tank's state message arrives. Nothing in any payload describes
the cadence: a consumer that cares works it out from consecutive
`timestamp`s.

- **At least every `sweep.interval`** (30 s by default; the "slow"
  interval). This is the promise consumers build staleness on, and it is
  the same with or without the fast cadence below.
- **As often as every `sweep.fast.interval`** (10 s in the example
  config), when `sweep.fast` is configured, while any probe is changing at
  `sweep.fast.trigger_c_per_min` or faster, and for up to `hold + decay`
  after the last such change, while the gap eases back to
  `sweep.interval`. Without `sweep.fast` the cadence is fixed, as it always
  was.
- **`timestamp` stays authoritative.** It is when the sweep started. Sweeps
  are never closer together than `sweep.fast.interval`, which is at least
  2 s, so two live state messages for one tank never share a `timestamp`
  (it is to the second). **A repeated `timestamp` still means a replay** of
  the retained message.
- **Staleness is still measured in slow intervals.** A state is stale once
  it's more than 3 × `sweep.interval` old (90 s by default). Faster sweeps
  only make data fresher, so nothing that was fresh becomes stale.
- **Why not a field?** State messages are retained. A field like "next
  sweep in 10 s" would be wrong for anyone who reads the retained copy
  later, and a field like "fast because a probe moved" is halfway to draw
  detection, which belongs downstream with the boiler events, not in
  tanktemp. The cadence is on tanktemp's own `/api/status` (`sweep`), for
  tuning, where it is computed per request.

## Delivery

What MQTT does and doesn't guarantee here:

- **Nothing is queued while the broker is unreachable.** A state message
  published while disconnected is dropped; the next sweep after the
  reconnect replaces it.
- **STARTUP can be lost.** tanktemp waits up to 10 s for the broker at
  start, then carries on without it; a STARTUP that couldn't be sent isn't
  retried. The HEARTBEAT after the connection comes up replaces it.
- **A SHUTDOWN that can't be sent becomes the last will.** Without a clean
  disconnect the broker publishes `LWT`, so a consumer sees `online:false`
  either way.

## Changes

- **DRAFT, not yet released: the water meter.** New topic `{prefix}/meter`
  (Meter, above); status gains `meters` (`[]` with none) and the
  `startup_problems` code `gpio_request_failed`. Tank state is unchanged.
  Consumers that decode status strictly must accept the new key.

- **2026-11-20, BREAKING, topics only:** the production prefix moved from
  `energy/hot_water` to `water/hot/tanktemp`, so the topics are now
  `water/hot/tanktemp/{device_id}/state` and `water/hot/tanktemp/status`.
  Payloads and goldens are unchanged. It was a clean cut, with no
  dual-publishing. The old retained messages were cleared once statehouse
  (the one consumer) had moved its `base_topic`. `water/hot/` is shared:
  washhouse publishes its derived topics there (`water/hot/state`,
  `water/hot/tank/{id}`, `water/hot/events`, `water/hot/status`), so
  tanktemp keeps its own `tanktemp` subtree, and its status and last will
  never collide with washhouse's. A water meter is planned at
  `water/hot/tanktemp/meter`, which `+/state` doesn't match.
- **2026-11-09, issue #1, no payload change:** state messages can arrive
  faster than `sweep.interval` while probes are changing (Cadence, above).
  No field was added or changed, and the goldens are unchanged. A
  consumer's staleness rule (3 × the slow interval) and replay rule (a
  repeated `timestamp`) both stand.
- **2026-11-02, before any consumer:** `order` counts from the **bottom**
  (1 = bottom), so it rises with `height_mm` and with the numbers on the
  probes. It was 1 = top. The `sensors` list is still sorted by `order`, so
  it now runs bottom to top.
- **2026-11-01, before any consumer:** `attempts` and `bus` are no longer
  in the state message. They were diagnostics (bus names change across
  reboots), and they're on tanktemp's own status page and `/api/status`
  instead.

## For consumers

`docs/STATEHOUSE.md` is the adapter spec built on this contract.
