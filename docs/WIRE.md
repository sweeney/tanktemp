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
| `{prefix}/status` | STARTUP, the first HEARTBEAT (`ready`), each scheduled HEARTBEAT, SHUTDOWN; and the last will | 1 | yes |

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
    {"position": "top", "order": 1, "height_mm": 1500, "rom": "28-0000117287e6",
     "temp_c": 61.185, "raw_c": 61.375, "offset_c": -0.19,
     "error": null, "suspect": null, "attempts": 1, "bus": "w1_bus_master2"}
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
| `sensors` | **every** configured sensor of this tank, in `order` (1 = top). A failing sensor is never left out |
| `position`, `order`, `height_mm`, `rom` | from the config. `height_mm` is `null` if not configured. The ROM is the physical sensor; it changes when a probe is replaced, while the position doesn't |
| `temp_c` | calibrated °C: `raw_c + offset_c`, at most 4 decimals. `null` when the read failed |
| `raw_c` | the sensor's own value, a multiple of 0.0625. `null` when the read failed |
| `offset_c` | the calibration applied, `0` when uncalibrated |
| `error` | `null`, or why there is no value: one of `not_present`, `bus_low`, `crc`, `power_on_reset`, `bad_scratchpad`, `out_of_range`, `malformed`, `read_error` |
| `suspect` | `null`, or `85c_unverified`: a value of exactly 85.000 °C that passed every check and a second read. It's probably real (a cylinder can reach it), but 85 °C is also the sensor's power-on value |
| `attempts` | reads it took |
| `bus` | the kernel's bus name, for diagnostics only: bus numbers change across reboots. `null` when the sensor is on no bus |

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
| `ready` | `true` once the first complete sweep has been published |
| `uptime_s` | seconds since start |
| `tanks` | the configured device IDs, in config order |
| `sensors_ok`, `sensors_failed` | from the last sweep; `null` before the first |
| `unexpected` | thermometers seen on a bus but not configured (a swapped probe shows up here). `null` before the first sweep |
| `startup_problems` | from the startup checks: `rom` (`null` for a bus-wide problem), `code` (`discover_failed`, `not_present`, `parasite_power`, `resolution`, `inspect_failed`), and a human `message` |
| `reason` | SHUTDOWN only: the signal (`SIGTERM`, `SIGINT`) or `context` |

The **last will** is exactly `{"online":false,"event":"LWT"}`, retained, QoS
1. The broker publishes it if tanktemp vanishes without a SHUTDOWN. It
carries no timestamp, because a will is fixed when tanktemp connects.

## For consumers

`docs/STATEHOUSE.md` is the adapter spec built on this contract.
