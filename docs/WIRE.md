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
`energy/hot_water`; test deployments use `test/energy/hot_water`.

| Topic | When | QoS | Retained |
|---|---|---|---|
| `{prefix}/{device_id}/state` | every sweep, one message per tank | 0 | yes |
| `{prefix}/status` | STARTUP, the first HEARTBEAT (`ready`), each scheduled HEARTBEAT, a HEARTBEAT after every MQTT reconnect, SHUTDOWN; and the last will | 1 | yes |

`device_id` is the tank's key in `devices_<site>`, so it is `[a-z0-9_-]+`
and safe as a topic segment. There is one tanktemp per site: a second one
would need its own prefix, because the status topic belongs to the daemon,
not to a tank.

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
  "tanks": ["hot-water-cylinder"],
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
| `startup_problems` | from the startup checks: `rom` (`null` for a bus-wide problem), `code` (`discover_failed`, `not_present`, `parasite_power`, `resolution`, `inspect_failed`), and a human `message` |
| `reason` | SHUTDOWN only: the signal (`SIGTERM`, `SIGINT`) or `context` |

The **last will** is exactly `{"online":false,"event":"LWT"}`, retained, QoS
1. The broker publishes it if tanktemp vanishes without a SHUTDOWN. It
carries no timestamp, because a will is fixed when tanktemp connects.

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
