# Hot-water meter: walkthrough

This follows one day of hot water through the meter, tanktemp, washhouse and
statehouse. The rules it applies come from
[`WATER-METER-PLAN.md`](WATER-METER-PLAN.md). The interactive version is
[`water-meter-explainer.html`](water-meter-explainer.html).

**§0 is real:** the meter's first night, 19–20 Nov 2026, replayed from raw edges.

**§1–7 are simulated.** Draw times and rates follow the washhouse explainer's scenario. The pulse times come from a 1 ms step simulation of a K=1 meter, using the plan's §6.4 rules. The temperatures are illustrative.

The real night showed the simulation's bath was far too slow (10 L/min, against a real 32–39). The kind rule now looks at flow first. The simulated day is otherwise kept as it was.

**Placement:** the combined hot outlet, as intended. The working assumption is the owner's: **the secondary loop does not draw through this pipe, so the meter sees draws only.** §5 shows what changes if the data says otherwise.

---

## 0. The real first night (19–20 Nov 2026)

The meter was fitted on GPIO 27 with only the Pi's internal pull-up, and every raw edge was recorded with no debounce. There were 277 edges in 15½ hours. All times are UTC, which is UK time in November.

| Time | What happened | Counted by the plan's counter |
|---|---|---|
| 17:29:47–17:30:52 | **A bath**, a pulse every 1.85 s (32 L/min). The reed was closed 0.40 s per turn. | 35 L, kind `bath` (flow ≥ 20 L/min) |
| 17:30:00.288 | **Noise:** one µs spike, 0.29 s after CH_ON. The reed was open, and the spike reported "opens". | rejected by the 20 ms debounce |
| 17:50:01–17:50:39 | **Bath top-up** at 36 L/min. | 23 L, `bath` (by volume alone it would be a tap) |
| 17:57:49–18:00:09 | **A slow tap** at about 4 L/min, a pulse every 12.9 s. The 18:00:02 litre is its tail: closed for 3.1 s, the tap's own speed. Before it, the wheel paused partway round, leaving the reed closed for 27 s. | 9 L, `tap` |
| 18:03:29 | **A lone close**, 207 s after the tap. It implies about 0.22 L/min, below Q1: creep, or a tap shut slowly. The pointer then rests on the magnet, with the reed closed until 22:29:56, and no chatter at all. | 1 L, no draw |
| 18:15:00.273 | **Noise at HW_OFF:** 6 spikes in 247 µs while the reed rests closed, each labelled "closes", then 2 more at +0.78 s and +0.80 s. | all rejected |
| 20:32:06 | **Noise**, one spike about 2 min after CH_OFF (the pump stopping after its run-on?). | rejected |
| 22:29:59–22:30:32 | **A sink.** | 2 L, `tap` |
| 23:02:44 → 07:37:52 | **Silence**, 8½ h. No loop flow through the meter, as far as the meter can tell. | – |
| 06:30:00.498, 06:34:53 | **Noise**, one spike each, the first at CH_ON. Nothing came at HW_ON at 06:15. | rejected |
| 07:37:45, 07:42:06 | **Two sinks.** | 2 L + 2 L |
| 07:48–07:54 | **Noise**, 3 spikes with no boiler event nearby. Cause unknown. | rejected |
| 08:50:23–08:58:26 | **A shower** at 7 L/min, a pulse every 8.8 s. | 56 L, `shower` |

**Totals:**
- **131 litres and 7 draws, with 0 phantom.**
- A counter of falling edges without debounce gets 140, with 9 phantom litres.
- Its worst case is the HW_OFF burst. The 600 ms backstop takes most of it, but not the pair 0.8 s later, which makes a **2 L "draw" at 76 L/min, called a bath**. The debounce is load-bearing.

**What the closed time adds.** The reed is closed for about 23 % of each turn, so tanktemp's `last_closed_ms` gives a flow figure as soon as the first litre's reed opens:

| Draw | Flow known from the closed time | Flow known from the second pulse |
|---|---|---|
| bath | 0.5 s | 1.8 s |
| shower | 1.4 s | 5.5 s |
| sinks | 3–4 s | 12–17 s |

**Still unchecked.** None of these litres is bucket-tested, and the meter's exact plumbing position is still to confirm (plan §2).

---

## 1. Overnight: silence

- No pulses. tanktemp's `trigger: sweep` meter message rides along with each probe sweep (every 30 s), with `count` unchanged.
- washhouse's `meter.state` stays `ok`. A silent meter is normal: only a draw seen by the probes with no pulses would make it suspect.
- At 06:30 CH comes on, and the cable picks up a spike (as on the real night, §0). The kernel's 20 ms debounce swallows it. tanktemp's `noise` stays 0 and `count` is unchanged.

```json
{"device_id":"hot_water_meter","timestamp":"2026-11-20T01:30:00Z","clock_synced":true,
 "trigger":"sweep","seq":1062,"litres_per_pulse":1,"level":"open",
 "counting_since":"2026-11-19T22:41:07Z","count":1640,
 "last_pulse_at":"2026-11-19T23:02:16.904Z","last_interval_ms":5821,
 "last_closed_ms":1339,"noise":0,"short":0,"glitches":0,"lost":0}
```

**This silence is itself the check on the loop assumption.** A thermosiphon through the meter would show here as a pulse every few minutes, all night (§5).

---

## 2. 02:15: the small nightly draw

A basin tap runs for 72 s at 2.5 L/min: 3 L in truth.

| Pulse | Time | Interval | washhouse |
|---|---|---|---|
| 1 | 02:15:07.2 | 3 h since the last | nothing yet: one pulse is never a draw on its own |
| 2 | 02:15:31.2 | **24 s** | short (< 36 s), so a candidate. **`draw_started`**, with `started_at` 02:14:55 |
| 3 | 02:15:55.2 | 24 s | `draw_updated` |
| – | 02:17:31 | 96 s with no pulse (4 × 24 s) | **`draw_ended`**, with `ended_at` 02:16:07 |

**Result:** `volume_l` **3 ± 1** (true 3), confidence 0.95, kind `tap`.
- The heat balance said about 2 L: it undercounts small draws, as the briefing says.
- `started_at` is 5 s early and `ended_at` 5 s early. **At slow flows, times are good to about half a gap between pulses** (12 s here).
- The end is *declared* 96 s after the last pulse, because a slow tap could still be running. statehouse already has the start, so that lag costs nothing.

---

## 3. 07:05: a shower, end to end

The shower opens at **07:05:00.0** and closes at **07:13:00.0**, drawing 9 L/min of **hot** water through the meter. That is 72 L in truth. A mixer shower takes only about 60–70 % of its flow from the cylinder, so this is a generous shower of about 13 L/min at the head.

**07:05:04.0, the first pulse** (`count` 1762). The litre pointer was 0.4 L round.
- *tanktemp* publishes it with `trigger: pulse`.
- **07:05:05.5:** the reed opens again after 1.5 s closed. The next message's `last_closed_ms` (1541) already says about 9 L/min (60 × 0.23 ÷ 1.54). washhouse can show a flow figure 5 s before the second litre arrives. It still waits for the second pulse to declare a draw.
- With the cadence flow-trigger PR, the probe sweeps go to 10 s **now**, not one or two sweeps later, when probe 1 has moved.
- *washhouse* does nothing yet.

**07:05:10.7, the second pulse** (interval 6.7 s, a candidate). washhouse publishes:

```json
{"schema":"net.swee.washhouse.event.v1","event":"draw_started",
 "draw_id":"d-20261120T070500Z","timestamp":"2026-11-20T07:05:10Z",
 "started_at":"2026-11-20T07:05:00Z","detected_at":"2026-11-20T07:05:10Z",
 "volume_source":"metered","volume_l":2,"flow_lpm":9.0,
 "masked_by_reheat":false,"confidence":0.95,"kind":null}
```

`started_at` = first pulse − ½ × interval = 07:05:00.7, which is 0.7 s after the tap opened.

*statehouse:*
- the gate passes: confidence 0.95 ≥ 0.7, and 2 L ≥ 0.5 L;
- `IngestSignal(…, Since = 07:05:00, ExpiresAt = +30 min)` and `RecordActivity`.

**The house is occupied 11 s after the tap opened.** That is about as fast as the thermal path, which often triggers on the first probe sweep after the tap opens (the explainer's simulation shows 0–20 s). **The meter doesn't win on speed; it wins on certainty and size.**

**Every 30 s after that: `draw_updated`,** with the running volume and about 9.0 L/min. statehouse refreshes the 30-minute expiry. Meanwhile, at 10 s sweeps:
- both probe 1s fall 15–19 °C within about 90 s, so both tanks are giving water;
- both probe 5s climb to the true top temperature, about 58 °C.

**07:12:57.3, the 72nd and last pulse** (`count` 1833).

**07:13:24, no pulse for 26.8 s** (4 × the 6.7 s median interval). washhouse publishes:

```json
{"schema":"net.swee.washhouse.event.v1","event":"draw_ended",
 "draw_id":"d-20261120T070500Z","timestamp":"2026-11-20T07:13:24Z",
 "started_at":"2026-11-20T07:05:00Z","ended_at":"2026-11-20T07:13:00Z","duration_s":480,
 "volume_l":72,"volume_uncertainty_l":3.2,"volume_source":"metered","volume_l40":115,"heat_balance_l40":128,
 "flow_lpm_mean":9.0,"flow_lpm_peak":9.0,"delivered_kwh":4.02,
 "outlet_c":58.0,"cold_c":10.0,
 "tanks":[
   {"device_id":"hot_water_cylinder_left","volume_l":32,"share":0.45,"share_source":"heat_balance"},
   {"device_id":"hot_water_cylinder_right","volume_l":40,"share":0.55,"share_source":"heat_balance"}],
 "masked_by_reheat":false,"kind":"shower","confidence":0.95,
 "assumptions":{"shower_lpm_min":6,"shower_lpm_max":12,"shower_min_s":180}}
```

How the numbers come out:

| Field | Working | Against the truth |
|---|---|---|
| `volume_l` | 72 pulses × 1 L | true 72; ±3.2 is 1 L of quantisation + 3 % (class 2, above 30 °C) |
| `delivered_kwh` | 72 L × (58 − 10) K × 4.186 / 3600 | |
| `tanks[].share` | the tanks' net ΔE was 1.8 and 2.2 kWh after loss | |
| `volume_l40` | 72 × (58 − 10) ÷ 30: the same draw in litres of 40 °C water, washhouse's usual unit | |
| `heat_balance_l40` | the thermal model's figure, kept for comparison | about 11 % high. In the meter explainer's simulated day, the heat balance ran from 22 % low to 33 % high on big draws |

*statehouse:* `ClearSignal`, and `UpdateActivity` with EndedAt and 72 L.

*washhouse, behind the scenes:*
- **A calibration sample.** The draw is clean (not masked, both tanks fresh, meter OK), so it records r = model ΔE ÷ delivered = 1.11 in `washhouse_calibration`.
- **A check on the estimate.** The fall in `usable_l40` is compared with 72 × 48/30 = 115 L of 40 °C equivalent.
- **Influx.** `washhouse_draw` gets the event, stamped at `started_at`.

---

## 4. 17:50: a bath during the evening reheat

HW and CH are both on, and the coils are heating. The bath runs at 10 L/min for 12 minutes: 120 L.

- **Before the meter,** this was the draw the thermal model couldn't size. Probe 1 sits in the coil zone, so the reheat hid the cold inlet's fall. It came out `masked_by_reheat: true` with `volume_l: null`, and statehouse's gate dropped it.
- **With the meter:**
  - 120 pulses at a median 6.0 s: **120 L**.
  - `masked_by_reheat` is still `true` (a fact), but `volume_source` is `metered` and confidence 0.95.
  - `kind` is `bath` by volume (≥ 80 L). The simulated bath runs at 10 L/min, much slower than the real one (§0: 32–39 L/min), which the flow rule alone would call a bath.
  - `tanks[].share_source` is `learnt`, because ΔE can't be split during a reheat. The share is the long-run split from clean draws.
  - It is not a calibration sample.
- **statehouse** gets an occupancy signal it never had before.

---

## 5. What if the data disagrees?

**A loop through the meter after all.**
- Overnight would show a pulse every 2–5 minutes, with probe 1 flat.
- After 2 nights of that, washhouse:
  - switches on the circulation baseline (plan §6.3);
  - starts subtracting it from draws: the 07:05 shower would read 75 pulses − 0.3 L/min × 8 min ≈ 72.6 L;
  - publishes `circulation_lpm`.
- That number would be **the first direct measurement of the gravity loop**: loop W ≈ flow × 4186 × (outlet − right P4).
- If it happens, revisit the cold-feed placement (plan §2, C).

**Draws partly bypassing the meter** (taps fed backwards up the loop's return).
- The metered litres would come out persistently below the heat balance, on clean draws, by more than the model's own bias.
- washhouse reports the ratio and doesn't act on it.

---

## 6. When things go wrong

**12:00, tanktemp is redeployed** and comes back 40 s later.
- The meter messages show a new `counting_since` (12:00:41) and `count` 0.
- washhouse closes the old series and records the 40 s as `unseen`. A draw in that gap is sized by the heat balance, with `volume_source: "heat_balance"`.

**A Saturday: the meter's cable is knocked off.**
- Probe sweeps continue, and the meter's sweep messages too, with the count frozen.
- At 09:12 a shower drops probe 1 by 17 °C with no pulse in 5 minutes. washhouse publishes `meter_suspect`, and `/healthz` turns `degraded`.
- That draw is sized thermally.
- The next metered draw that the probes corroborate clears it, with `meter_ok`.

**A hot tap left dribbling** at 1.2 L/min.
- Net flow continues with no 15-minute gap. After 2 hours washhouse publishes `continuous_flow_started`, with its rate and 144 L so far.

**Noise when the boiler switches.** This replaces "reed chatter": the real reed doesn't bounce, even resting on the magnet for hours (§0). The problem is electrical.
- At 17:00 HW comes on, and at 18:15 it goes off. The cable, running beside bonded copper on a weakly pulled-up pin, picks up a burst of µs spikes.
- Each spike returns to the old level within microseconds. The kernel's 20 ms debounce only reports a level that holds, so the spikes never reach tanktemp. `count`, `noise` and `glitches` stay unchanged.
- **If the debounce were off**, every spike's falling flank would look like a litre. 18:15 would add 2 phantom litres 0.8 s apart: a 2 L "draw" at 76 L/min, which the flow rule would call a bath. That is why config refuses `debounce: 0`.
- If `noise` ever climbs, washhouse lines the increments up with HW and CH events and publishes the statistic. A rise after the input board is fitted is a wiring fault, not water.
- An edge closer than 600 ms after the last counted one is still a `glitch` (the backstop). A lone pulse is still never a draw without a thermal signature.

---

## 7. What the day adds up to

| | |
|---|---|
| `today_l` | the sum of every draw's metered litres: no estimate, and no draw missing |
| Draws | every one has a metered volume, including the bath that the reheat hid from the heat balance |
| Calibration | about 4 clean draws a day. Within a fortnight there's enough to test the guessed vessel geometry, and to measure the flow rates `kind` is judged by |
| The loop question | answered by the night's silence, or otherwise |

The meter turns washhouse's volume estimates into measurements. The probes keep the jobs only they can do: which tank, how hot, and how much is left.
