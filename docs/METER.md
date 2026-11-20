# Counting a reed water meter on a Raspberry Pi: notes for tanktemp

How tanktemp counts the hot-water meter, and everything learned about it on
the rig. It's the meter's counterpart to [`ONEWIRE.md`](ONEWIRE.md).

- **The contract** for `{prefix}/meter` is [`WIRE.md`](WIRE.md), Meter. That
  document is normative; this one explains how the numbers there are made.
- **The plan** it grew from, including how washhouse uses the meter, is
  [`WATER-METER-PLAN.md`](WATER-METER-PLAN.md). A worked day is in
  [`WATER-METER-WALKTHROUGH.md`](WATER-METER-WALKTHROUGH.md), and an
  interactive replay of the first night is in
  [`water-meter-explainer.html`](water-meter-explainer.html).
- **The defaults** are guesses, recorded in [`GUESSES.md`](../GUESSES.md)
  as G54–G59.

## The rules, short version

1. **Connect or disconnect the meter leads only with the Pi powered off.** A
   Pi died when they were pushed onto the header live.
2. **tanktemp publishes raw facts:** the count, when the last litre came,
   how long the reed stayed closed, and why edges were rejected. Flow,
   draws and circulation are for consumers.
3. **The kernel debounce is mandatory** (`meter.debounce`, at least 5 ms;
   default 20 ms). On today's wiring, the boiler's switching puts µs spikes
   on the line. Without the debounce, those spikes reach the counter.
4. **The edge's label is never trusted on its own.** One litre is one
   change from open to closed, judged on the level read after each edge.
5. **Never count across lost edges.** After a gap in the kernel's event
   numbers, tanktemp resyncs to the line's level. A litre may be lost, never
   invented.
6. **Intervals are on the monotonic clock**, from the kernel's own event
   stamps, so NTP stepping the wall clock at boot can't fake or hide a
   litre.
7. **Nothing is persisted.** The count starts at 0 when tanktemp requests
   the line, and `counting_since` says when.
8. **Only `internal/gpioedge` knows about GPIO.** The counter
   (`internal/meter`) is pure, and sees the line only as a `meter.Source`.

## Hardware

### The meter

| | |
|---|---|
| Type | WMP20H, a pulse-output volumetric meter with a reed switch: the litre pointer carries a magnet that closes the reed once per turn |
| K | 1 L per pulse (`litres_per_pulse: 1`) |
| Q4 (overload) | 5 m³/h, 83 L/min: a litre every 0.72 s at most. `min_interval` must stay below that (the config refuses more than 700 ms) |
| Q1 (minimum) | about 1.67 L/min. It is fitted vertically, which drops it one class (R80 to R40, GUESSES G58). Below Q1 it still turns, just less accurately |
| Duty | the reed is closed for about **0.23 of each turn** (G57). 0.25 was measured live on a 6.3 L/min tap. So `last_closed_ms` gives the flow within one litre |
| Bounce | none seen. Every real closing and opening is one clean edge; the shortest real closed time seen was 343 ms |

### Wiring

```
 Pi header                         meter cable (2 m)
 pin 13  GPIO 27  ─────────────── red   ──┐
                                          reed (dry contact)
 pin 14  GND      ─────────────── black ──┘
```

- **The pull-up is the Pi's internal one only**, about 50 kΩ, which
  tanktemp sets when it requests the line. Open reads high, closed low.
- **There is no RC filter.** A small conditioning board (a 10 kΩ pull-up,
  then 10 kΩ and 100 nF in series) is specified in WATER-METER-PLAN §3,
  but isn't fitted. The software must count correctly without it.
- **GPIO 27 is free.** 1-Wire uses 4 and 17, and SPI0 uses 7–11. The
  config takes BCM lines 2–27, and refuses 0 and 1: those are the HAT ID
  EEPROM pins, which the firmware probes at boot.
- **No `config.txt` change is needed.** There is no overlay: tanktemp
  requests the line through the GPIO character device at start, with the
  pull-up, both edges and the debounce.
- **To check:** whether the reed is isolated from the pipe (multimeter).
  If it isn't, the dry contact is tied to bonded earth.

### Electrical noise

The meter's copper pipe is earth-bonded to the boiler's primary pipework,
and the cable runs beside bonded copper. While the boiler's programmer
switches heating or hot water, spikes of a few µs reach the
high-impedance pin. For example, at one HW_OFF: 6 spikes in 247 µs, then
2 more about 0.8 s later.

- **Each spike reports the level the line already had.** The kernel reads
  the pin after the interrupt, by which time the spike is over: the event's
  id is that level, not the edge that fired it (checked in
  `gpiolib-cdev.c`). So with no debounce, a spike shows up as a "closing"
  while the reed is already closed. Counting falling edges would count it;
  judging on the level makes it `noise`.
- **With the kernel debounce, spikes never reach user space.** The debounce
  only reports a level that has held for the whole period. So `noise` is
  expected to stay 0 in production; a rise means the debounce isn't
  holding.

## The Pi: permissions and the unit

- **`/dev/gpiochip0` is `root:gpio 0660`.** Raspberry Pi OS makes the
  `gpio` group. `/dev/gpiochip4` is a link to it on this Pi.
- **The `tanktemp` user is not in `gpio`.** The unit adds the group for the
  service only (`SupplementaryGroups=gpio`). `setup-pi.sh` and `deploy.sh`
  both refuse a Pi with no `gpio` group, before changing anything. Without
  the group, systemd can't start the unit at all: probes, meter and
  everything.
- **The unit's device access.** `PrivateDevices=true` would hide
  `/dev/gpiochip*`, so the unit uses instead:
  - `DevicePolicy=closed`: no device but the standard pseudo-devices;
  - `DeviceAllow=char-gpiochip rw`: the GPIO chips;
  - the rest of what `PrivateDevices` gave, spelled out:
    `SystemCallFilter=~@raw-io`, `SystemCallArchitectures=native` and
    `InaccessiblePaths=-/dev/shm`.

  `systemd-analyze security` scores it 3.6, against 3.7 for the unit
  before the meter (lower is better).
- **Without a `meters` block, no chip is opened.**
- **Only one process can hold a line.** While tanktemp runs, `gpioinfo`
  shows the line as taken:

  ```
  gpiochip0 27  "GPIO27"  input bias=pull-up edges=both debounce-period=20ms consumer="tanktemp-meter"
  ```

  `pulsewatch`, `gpiomon` and the hardware check all need tanktemp
  stopped. Anything else holding the line makes tanktemp's request fail
  with "device or resource busy". tanktemp then reports
  `gpio_request_failed` and carries on with the probes alone.

## The character device interface (`internal/gpioedge`)

`gpioedge.Open(chip, offset, debounce)` requests the line through
`go-gpiocdev`:

- as an input, with the pull-up;
- on both edges;
- with the kernel debounce, refused below 5 ms;
- with event timestamps from `CLOCK_MONOTONIC`;
- under the consumer name `tanktemp-meter`.

It reads the level once at the request (`Initial`), then forwards each
event to a channel as a `meter.Edge`: the monotonic time, the level, and
the kernel's line sequence number.

What the kernel does, which the counter relies on:

| Behaviour | Consequence |
|---|---|
| The event id is the level the pin reads, after the interrupt | a spike reads as the level the line already had: `noise`, not a litre |
| With debounce, an event is sent only when the debounced level **changes** | same-level events (`noise`) are expected only without debounce |
| A debounced event is stamped when the debounce completes | every meter time is about `debounce` (20 ms) after the reed moved. Intervals and closed times are unaffected |
| The event buffer (a kfifo) drops its **oldest** event when full | the line sequence number jumps: `lost`. tanktemp's own handler is non-blocking, with a 256-event buffer, and expects 0 |
| Event stamps are `CLOCK_MONOTONIC` | `gpioedge.Now` reads the same clock, so tanktemp can turn an edge's time into wall time: wall now − (mono now − edge time) |

`internal/gpioedge` is the only package that imports `go-gpiocdev` or
`golang.org/x/sys/unix`. Off Linux it builds a stub, so the rest of
tanktemp builds and tests on a Mac.

## Counting (`internal/meter`)

`meter.Counter` is a pure state machine. It starts from the level read at
the request, then takes each edge with `Observe` and the passage of time
with `Tick`. It checks, in this order:

1. **lost:** a gap in the sequence numbers. It takes the edge's level as
   the line's level, drops any closing still being held, and counts
   nothing across the gap. A repeated or backwards number is `noise`.
2. **noise:** an edge that doesn't change the level.
3. **short:** a closing that reopens within `meter.hold` (20 ms). Neither
   edge counts.
4. **glitch:** a held closing less than `meter.min_interval` (600 ms) after
   the last counted litre. Not counted.

A closing that passes all four is **counted once it has held for
`meter.hold`**. It's confirmed at the next edge or tick, and tanktemp ticks
every 50 ms (G59), so a `pulse` message follows its closing within `hold` +
50 ms. The opening that ends a counted litre emits `open`, with
`last_closed_ms`.

`meter.Loop` is the one goroutine that owns the counter. It selects over
edges, ticks and requests (a snapshot for `/healthz`, a sweep's snapshot),
and **drains every queued edge before handling a tick or a request**. So a
tick can't confirm a closing ahead of the edge that would have made it
`short`, and a snapshot never misses an edge already received. Each event
carries its own snapshot, taken as it happened.

`cmd/tanktemp` turns events into `{prefix}/meter` messages off the loop:

- through a bounded queue of 1,024 that drops the oldest when the broker
  is too slow;
- a waiting `sweep` message is replaced by a newer one;
- `seq` is assigned as each message leaves the queue, so it has no gaps of
  tanktemp's making;
- the queue is flushed (for up to 2 s) before SHUTDOWN.

`/healthz` and `/api/status` report each meter's state:

| State | Meaning |
|---|---|
| `starting` | tanktemp is starting; the line isn't requested yet |
| `counting` | the line is requested and the loop running |
| `failed` | the line couldn't be requested (`gpio_request_failed`); see `reason` |
| `stopped` | the line stopped while running; see `reason` |

Alongside the state: the counters, `publish_dropped` (messages dropped by
a slow broker, not litres) and `queued`.

## Reading the numbers

| You see | It means |
|---|---|
| `level: closed` for hours, `last_closed_ms` in minutes or hours | the pointer stopped with its magnet on the reed. Not a fault, and not a flow. The rig rested closed for 4 h 26 m on its first night |
| `count` jumps by more than one between messages | messages were dropped (`publish_dropped`) or lost in delivery. `count` is still right |
| a new `counting_since` | tanktemp restarted, or requested the line again. Anything between the two runs went unseen |
| `noise` rising | same-level events reached user space: the debounce isn't holding |
| `short` or `glitches` rising | spikes long enough to flip the debounced level. Not counted, but worth a look: see the next section |
| `lost` > 0 | the kernel dropped edges. `count` is a lower bound, short by at most 2 × `lost` (WIRE.md) |
| `gpio_request_failed` in `startup_problems`, meter state `failed` | the line is busy (another holder), not permitted (the unit or group), or the chip wasn't found. Probes still publish; `/healthz` is `degraded` |

### Creep, and a pointer parked at the magnet's edge

Through 20 Nov, single litres came every 8–80 minutes during the day, and
once the reed read closed for 12 minutes, with same-level spikes 1.6–10 s
apart and no boiler event near them. washhouse reads that as a slow creep,
about 0.1 L/min: secondary circulation through the meter, or a drip. The
pointer sat at the edge of the magnet and chattered.

Today those chatters are µs long, and the 20 ms debounce swallows them. A
chatter that opened for 20 ms or more and then closed would count as a
litre, and no counter would show it: it holds (so not `short`), comes more
than 600 ms after the last litre (so not a `glitch`), and is a real change
of level (so not `noise`). washhouse's audit flags any litre whose opening
before it lasted under 300 ms; a real litre is open at least about 0.55 s,
even at Q4. None has been seen. If one is, the fix in the counter is a
minimum open time before a closing can count.

## What the rig has shown

| When | What | Result |
|---|---|---|
| 19–20 Nov, 17:29–09:08 UTC | raw `gpiomon` capture, no debounce: 277 edges, committed as `internal/meter/testdata/first-night` | 131 litres, 0 phantom. 15 spikes in 8 clusters, all rejected. Counting falling edges would have given 140 |
| 19–20 Nov, 17:29–22:03 UTC (29 h) | the same capture, continued: 657 edges, replayed offline | 305 litres raw, and 305 with the spikes removed as the kernel debounce would. All 47 spikes rejected; the shortest interval between litres was 1.40 s (a bath). washhouse's independent decoder agrees |
| 20 Nov, 22:09 UTC | tanktemp counting live, debounce 20 ms | a tap run of 6 litres, 9.2–10.3 s apart (6.3 L/min), closed 2.3–2.6 s each (duty 0.25). `noise`, `short`, `glitches` and `lost` all 0 |

Measured flows from the first night: bath 32–39 L/min, shower 7 L/min,
sinks 4–5 L/min. Overnight there was no litre at all for 8½ hours.

## Tools

- **`cmd/pulsewatch`** watches the line and prints what the counter makes
  of each edge: counted (with its closed time), noise, short, glitch or
  lost, then a summary. It runs the same `gpioedge` source and
  `meter.Counter` as tanktemp, with the same 50 ms tick. It's the tool for
  the bucket test and for tuning `debounce`, `hold` and `min_interval`. It
  only reads, and it needs the line free, so stop tanktemp first.

  ```sh
  GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -o pulsewatch ./cmd/pulsewatch
  # copy to the Pi, stop tanktemp, then:
  ./pulsewatch -for 2m                     # GPIO 27 on gpiochip0, the config's defaults
  ./pulsewatch -gpio 27 -debounce 10ms -hold 20ms -min-interval 600ms
  ```

- **The hardware check** (build tag `hardware`, never run in CI) requests
  the real line and runs `metertest.Contract` against it, then checks that
  a held line can't be requested twice. With `TANKTEMP_METER_WATCH` it
  also prints every edge for that long, while you run a tap:

  ```sh
  GOOS=linux GOARCH=arm GOARM=6 go test -c -tags hardware -o gpioedge.test ./internal/gpioedge
  # on the Pi, with tanktemp stopped:
  TANKTEMP_METER_LINE=gpiochip0:27 ./gpioedge.test -test.v
  TANKTEMP_METER_LINE=gpiochip0:27 TANKTEMP_METER_WATCH=60s ./gpioedge.test -test.v -test.run Watch
  ```

- **A raw capture for a new fixture** (no debounce, so spikes are kept):

  ```sh
  gpiomon -c gpiochip0 -b pull-up -E realtime -C watermeter -F '%S %e' 27 > gpio27-<UTC start>.edges
  ```

  The format: one edge per line, `epoch_seconds.ns edge`, where 1 is
  rising (the reed opens) and 2 is falling (it closes); `#` lines are
  comments. A new recording becomes a directory under
  `internal/meter/testdata/` with its `gpio27.edges` and an `expect.json`
  that fails before any fix, as for the 1-Wire fixtures. Expected values
  come from an analysis made independently of the counter (a time-aligned
  boiler log and the draws), never from running the counter itself.

## Not yet verified

- **The bucket test** (deferred): draw a measured 10 L from a full-bore
  tap, three times, and compare `pulsewatch`, the published `count` and the
  dial. No litre has been checked against a bucket yet.
- **The kernel debounce across boiler switching, live.** The replays show
  that removing the same-level spikes gives the right count. The overnight
  audit from 20 Nov (`noise`, `short`, `glitches` and `lost` all 0 across
  every HW and CH switch) is what will confirm the kernel does that on this
  Pi.
- **Whether the reed is isolated from the pipe**, and what the cable
  label says.
- **The conditioning board**, which isn't fitted yet. Once it is, the
  spikes should stop at the pin.
- **Where the meter sits relative to the secondary return**
  (WATER-METER-PLAN §2): whether loop flow can pass through it.
