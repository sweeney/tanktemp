# A statehouse adapter for tanktemp: specification

This is for statehouse to implement a `tanktemp` adapter, following its
`boiler`, `ups` and `climate` adapters. tanktemp itself has no statehouse
code. The MQTT contract this builds on is [`WIRE.md`](WIRE.md), which is
normative; this document says what statehouse should do with it.

Status: **spec, not implemented.** tanktemp publishes under
`test/energy/hot_water` until it has been reviewed; `energy/hot_water` is
the production prefix.

## What tanktemp publishes, in one paragraph

Every 30 s it reads every DS18B20 probe on every configured cylinder. For
each cylinder it publishes one retained message on
`{prefix}/{device_id}/state` (QoS 0). The message lists **every** configured
probe, top first, each with its calibrated temperature or `null` and an
error code. On `{prefix}/status` (QoS 1, retained) it publishes STARTUP, a
HEARTBEAT when the first sweep is in, a HEARTBEAT every 15 minutes, and
SHUTDOWN. Its last will on that topic is `{"online":false,"event":"LWT"}`.

## Subscriptions

```
{base}/+/state
{base}/status
```

`base` is configurable, as for the other adapters, and defaults to
`energy/hot_water`. Use exactly these two patterns rather than `{base}/#`,
so a future topic doesn't arrive unannounced.

## Identity: one engine device per probe

```
scheme  = "tanktemp"
primary = "<device_id>.<position>"     e.g. "hot-water-cylinder.top"
```

- **A probe, not a tank, is the engine device.** Each probe is its own
  `temperature_c` series in `device_environment`, and greenhouse charts one
  series per device. A tank-level device would need a field per position
  (`temperature_top_c`, …), which fits neither the fixed field names nor
  "any number of probes per tank".
- **The identity is the position, not the ROM.** When a dead probe is
  replaced at the same position, the ROM changes and the series continues.
  When a probe is moved to a new height, it gets a new position and so a new
  series, which is right: it now measures somewhere else.
- **Carry the ROM as metadata,** so a swap is visible (for instance as a
  "probe replaced" note when the ROM under a primary changes).
- `device_id` is `[a-z0-9_-]+` and `position` is `[a-z0-9_]+`, so the `.`
  separator can't be ambiguous.

## devices_<site> entries

The cylinder gets one entry, and each probe gets its own, related by
`part_of`. Both `part_of` and `telemetry` belong to the `devices` follow-up
(tanktemp's PLAN.md, "common/devices follow-up"); until statehouse reads
them, they decode as unknown keys. The example is made up:

```json
{
  "hot-water-cylinder": {
    "class": "hot_water_cylinder",
    "display_name": "Hot water cylinder",
    "telemetry": false,
    "room": "first.cupboard",
    "floor": "first",
    "manufacturer": "Example Co",
    "model": "UV-210",
    "installed": "2019-05-14",
    "cylinder": {"capacity_l": 210, "height_mm": 1480, "diameter_mm": 550}
  },
  "hot-water-cylinder-top": {
    "class": "appliance_probe",
    "display_name": "Cylinder: top",
    "scheme": "tanktemp",
    "primary": "hot-water-cylinder.top",
    "environment_fields": ["temperature_c"],
    "part_of": "hot-water-cylinder"
  }
}
```

- **Class `appliance_probe`** (decided 2026-10-31). The descriptions are
  being widened to cover cylinder probes in sweeney/statehouse#79 and
  sweeney/greenhouse#45. `IsPassiveSensor` already covers the class, and
  greenhouse already charts it without ever averaging it into a room. What
  tells a cylinder probe from a fridge probe is `part_of` pointing at a
  `hot_water_cylinder`.
- **`target_temperature` is optional on a cylinder probe,** relayed raw like
  the fridge and freezer targets. What counts as too cold for stored hot
  water is the consumer's policy.
- **The entries must exist before the first production deploy.** A probe
  with no entry resolves to `unclassified` (or a name hint) in
  `internal/device/profiles.go`, and greenhouse doesn't chart `unclassified`
  at all, so the probes would be invisible.
- **The cylinder entry has `telemetry: false`:** it sends nothing itself.
  statehouse should leave such entries out of anything about liveness or
  readings ("never heard from", online counts) once it reads the key.

## Mapping a state message

For each element of `sensors`:

| tanktemp | statehouse |
|---|---|
| `device_id` + `.` + `position` | `primary` (scheme `tanktemp`) |
| `temp_c` | `temperature_c` in `device_environment`, at `timestamp` |
| `temp_c: null` | **write nothing.** Never write 0. Count or log `error` |
| `error` | the reason there's no value: `not_present`, `bus_low`, `crc`, `power_on_reset`, `bad_scratchpad`, `out_of_range`, `malformed`, `read_error`. Match on these codes; they are stable |
| `suspect: "85c_unverified"` | the value is published and probably real; keep the flag if statehouse can carry per-reading quality, otherwise write the value |
| `rom` | metadata (see Identity) |
| `raw_c`, `offset_c` | publisher metadata. `temp_c` already includes the calibration; `raw_c` is for diagnosis. Statehouse need not keep either |
| `order`, `height_mm` | config facts. Useful for a stratification view; not needed for the series |
| `attempts`, `bus` | diagnostics only. Drop them. Bus names change across reboots |

The **message-level** fields:

| Field | Use |
|---|---|
| `timestamp` | the reading time for every probe in the message (UTC, to the second) |
| `clock_synced` | if `false`, the Pi's clock hadn't synced via NTP, and the timestamp may be wrong (the Pi has no real-time clock). Recommended: use the broker receive time instead, or drop the reading, for these messages. This mostly happens in the first minute after a reboot |
| `sweep_ms` | diagnostics only |

### Retained messages and staleness

Both topics are retained, so on (re)connect statehouse gets the last state
of every cylinder at once. **The retained flag says nothing about age; the
`timestamp` does.** Recommended: accept a retained state as the current
value only if its `timestamp` is within 3 sweep intervals (90 s) of now.
Otherwise mark those devices stale, or wait for the next live message.
Never write a retained message's reading into `device_environment` twice:
tanktemp republishes every 30 s, so a duplicate timestamp means a replay.

## Mapping the status topic

| Message | Meaning for the engine |
|---|---|
| `{"online":true, "event":"STARTUP", …}` | tanktemp started. `ready` is false; no readings yet |
| `{"online":true, "event":"HEARTBEAT", "ready":true, …}` | alive, with readings. The first one follows the first sweep, then every 15 min, and after every MQTT reconnect |
| `{"online":false, "event":"SHUTDOWN", "reason":"SIGTERM", …}` | stopped cleanly. Mark every `tanktemp` device offline |
| `{"online":false, "event":"LWT"}` | the broker lost tanktemp. Mark every `tanktemp` device offline |

`sensors_ok`, `sensors_failed`, `unexpected` (ROMs on a bus but not
configured: usually a probe swapped without a config change) and
`startup_problems` are publisher health, like boiler-sensor's metrics: not
house state. Statehouse may surface them on its own status page; it should
not create devices from them.

Like boiler's adapter, the status topic says the **publisher** is alive.
Per-probe liveness comes from the state messages: a probe whose `error` is
non-null in every message for N minutes is down, even while tanktemp is
online.

## Compatibility

- The goldens in `internal/payload/testdata/` are the contract. New fields
  are additive, and an adapter must ignore unknown fields.
- Renaming or removing a field, or changing an error code, is a major
  change, announced in WIRE.md first.
- The topic prefix will move from `test/energy/hot_water` to
  `energy/hot_water` once reviewed. Keep the base configurable.

## A worked example: the data-line fault

This fault actually happened on the test rig (`docs/samples/fault-data-line.csv`):
the pull-up contact failed for about 10 minutes, then recovered. It is
replayed as `TestFaultReplays/data_line` in `internal/status`. The three
probes are `cyl.left`, `cyl.middle` and `cyl.right`. The values are the
CSV's rows at 16:51 and 17:02:30, as tanktemp publishes them: exact
sixteenths, where the logger's CSV shows the kernel's truncated `t=`
(20.5625 there is 20.562).

| Sweep | What tanktemp publishes (per probe) | What statehouse should do |
|---|---|---|
| 1 | `temp_c` 20.5625 / 20.3125 / 20.25, `error` null | write three `temperature_c` points |
| 2 | `temp_c` null ×3, `error` `bus_low` ×3 (the data line was held low; the kernel's raw value was a false 0.000) | write nothing; note `bus_low` |
| 3 | `temp_c` null ×3, `error` `not_present` ×3 (the sensors dropped off the bus) | write nothing; after N minutes of this, the probes are down |
| 4 | `temp_c` 20.5625 / 20.25 / 20.1875, `error` null | write three points; the probes are back |

The status topic stays `online:true` throughout: the publisher was fine,
the wiring wasn't. That is why per-probe liveness has to come from the
state messages.

## Test vectors

`internal/payload/testdata/*.golden` are exact payloads to use as adapter
test fixtures: `state-ok`, `state-failures` (a suspect 85 °C, a probe
`not_present` with `bus: null`, a `crc` failure, `clock_synced: false`),
`status-startup` (with a bus-wide startup problem whose `rom` is `null`),
`status-heartbeat`, `status-shutdown` and `lwt`.
