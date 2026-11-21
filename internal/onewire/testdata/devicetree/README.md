Device-tree fixtures for `onewire.BusPins` (#10): just the nodes it reads,
under `sys/firmware/devicetree/base/`.

- `rig-2026-11-21`: captured from the rig. `onewire@4/gpios` and
  `onewire@11/gpios` are the bytes `od -tx1` showed on 2026-11-21: phandle 7
  (the GPIO controller), the pin (4, 17) and flags 0, each a big-endian
  32-bit cell. `soc` stands for every other node.
- `none` (synthetic): no 1-Wire overlay loaded.
- `malformed` (synthetic): a truncated `gpios`.
