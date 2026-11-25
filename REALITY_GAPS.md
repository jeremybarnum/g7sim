# Where the emulator differs from a real G7

A gap is a difference between the emulator and a real sensor as we understand it. Each entry says what a real G7
does, what the emulator does, how we know, and what it hid or broke in testing. Tooling problems that don't change
what a client sees are listed separately at the end.

Status: open unless marked otherwise. Candidates for one pull request.

## Gaps

### 1. A new display is never asked for a Bluetooth bond

- **Real G7:** after a display completes the key exchange with the pairing code, the sensor answers `bond=2` and, at
  the end of the handshake, sends a Bluetooth security request (just-works pairing). If the display's system doesn't
  complete the bond, the sensor drops the link and refuses the display's new key at the next connection, so the
  display must pair again. On an Apple Watch the system shows a pairing prompt only while the app is in front with
  the screen lit; otherwise it ignores the request ("no sessions that support it", error 4815).
- **Emulator:** answers `bond=0` (`verdictPaired = {0x01, 0x00}` in `g7/sensor.go`), and `finalize()` in
  `g7/handler.go` replies to the bond request with an in-protocol "bond confirmed" and logs "paired". It never sends
  a Bluetooth security request and keeps the key whatever happens.
- **Seen:** 11-24, real sensor DXCMuz, Jeremy's watch: six first contacts in the background and one with Loop in
  front but the screen off all failed this way; the one with the screen on got the prompt, was accepted, and bonded
  (22:01:13). Watch app log and watch sysdiagnoses (bluetoothd), `~/Documents/LoopSysDiagnose/2026-11-24-real-g7-dexcom-pairing`.
- **Hid:** every emulator run showed the watch's first contact succeeding from behind the clock (11-23, 11-24), which
  led to wait screens that tell the user nothing is needed.
- **Fix, cheap:** a switch that answers `bond=2` and then behaves like a sensor whose bond never completed (drop the
  link, forget the key). Reproduces the failure loop; can't show success.
- **Fix, full:** real SMP pairing in the emulator's own Bluetooth stack (it runs on a raw HCI channel, so the kernel's
  pairing is bypassed), and forget the key unless the bond completed.

### 2. Dexcom-app mode: no answer to the key-challenge nonce

- **Real G7:** answers every step of a display's pairing.
- **Emulator:** on 11-24 17:02:20 it didn't answer the watch's last pairing message (key challenge nonce); on 11-23 it
  answered in 65 ms.

### 3. A missed disconnect stops advertising for good

- **Real G7:** keeps its advertising schedule whatever a display does.
- **Emulator:** missed the watch's disconnect (11-24 17:03:11) and never advertised again, locking out the phone too.

### 4. Dexcom-app mode: the 2-second listener guess

- **Real G7:** tells displays apart by the key each one authenticates with, not by timing.
- **Emulator:** treats a client that doesn't write within 2 s as a listener and hands it the reading. A suspended
  (slow) watch gets taken for a listener, so its pairing round 1 gets no answer (11-23 20:36:42).

### 5. Dexcom-app mode: one display's reading booked to another's slot

- **Real G7:** each display type collects its own reading.
- **Emulator:** booked the phone's reading to the watch's slot (display type 3, 11-24 17:34:13), then dropped the watch
  twice with "display type 3 already collected".

### 6. Dexcom-app mode: keeps advertising as unpaired after serving

- **Real G7:** goes quiet once its displays have the reading.
- **Emulator:** the simulated Dexcom app holds no slot, so the emulator keeps advertising as unpaired and the phone
  reconnects 1–3 times a second, refused each time ("already served") (11-23 20:32).

### 7. Only G7SensorKit's handshake is answered

- **Real G7:** pairs with the real Dexcom app.
- **Emulator:** answers only G7SensorKit's messages, so the real Dexcom app connects, gets the certificates and never
  finishes ("Searching for sensor", 11-23 20:09).

### Suspected (not yet shown to differ from a real G7)

- **8.** In Dexcom-app mode the emulator waits 65 s for a client to turn on the authentication notifications; Loop
  never did (11-23 19:59). Whether a real G7 waits the same way is unknown.
- **9.** The phone's pairing scan never saw 6 of 12 emulated sensors (11-22), though the emulator advertised the whole
  time. Cause unknown; may be the emulator's advertising.
- **10.** "advertising full: HCI command 0x200a return 0x0C" each time the set of display types in use changes
  (11-24 17:34:13, 18:23:35). Effect on what clients see is unknown.

## Tooling (not gaps)

- `POST /new` hung once in `radio.Stop()` (11-24 15:36), and its re-exec drops `-dexcom-app`. Workaround: stop the
  service, move `g7state.json` aside, put fresh-sensor flags in `~/g7sim/fresh-args` (one-shot, read by `g7svc.sh`),
  start the service.
- Noted 11-23: the binary deployed on 11-07 (`4220ed2a` plus uncommitted edits) had no source on record; `0d69447` (11-07 13:33) may be it. Check before building on it.
