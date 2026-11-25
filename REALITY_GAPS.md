# Where the emulator differs from a real G7

A gap is a difference between the emulator and a real sensor as we understand it. Each entry says what a real G7
does, what the emulator does, how we know, and what it hid or broke in testing. Tooling problems that don't change
what a client sees are listed separately at the end.

Status: open unless marked otherwise. Candidates for one pull request.

## Gaps

### 1. A new display is never asked for a Bluetooth bond

- **Real G7:** after a display completes the key exchange with the pairing code, the sensor's verdict is `05 01 02`
  (authenticated, no Bluetooth bond). The display writes the keep-alive (`06 1E`, answered `06 00`) and the bond
  request (`07`). The sensor answers `07 00` and, in the same millisecond, sends a Bluetooth security request (LE
  Secure Connections, just-works pairing). Then one of three things happens:
  - the display's system pairs: the link is encrypted, the sensor sends `08 01` about 0.1 s later, and the display
    reads glucose from then on (verdict `05 01 01` at later connections, which the display encrypts within
    milliseconds of connecting);
  - the system refuses (an Apple Watch or iPhone whose app is not in front answers Pairing Failed, reason 0x08,
    within 1 ms; bluetoothd logs "Ignoring security request as there are no sessions that support it", error 4815):
    the sensor hangs up about 0.2 s later;
  - the system shows its Pair prompt (app in front) and nobody answers: the sensor hangs up about 31 s after its
    Pairing Response (error 719 on the watch).
  An unbonded display gets no glucose. Its key is not thrown away at once: in direct mode the same never-bonded key
  was accepted again (`05 01 02`, then a new bond request) 44 s to 2 min 54 s later, at every connection until it
  bonded; with the Dexcom app on the phone it was refused 4 min 22 s and 14 min 48 s after a failed bond, and the
  display had to run the key exchange again. A lapse after a few minutes without a connection fits both; the rule
  is not settled. An unbonded display was sometimes offered a connection every minute instead of every five (cause
  unknown). A bonded display that loses its system bond (sensor forgotten in Bluetooth settings) keeps its key and
  gets `05 01 02` and a new bond request.
- **Emulator:** answers `05 01 00` to a new key (`verdictPaired = {0x01, 0x00}` in `g7/sensor.go`) and `05 01 01` to
  every stored key; there is no "key valid, not bonded" state. `finalize()` in `g7/handler.go` answers the bond
  request with `07 00` and at once `08 00`, and logs "paired". It never sends a security request, its links are never
  encrypted, and it serves glucose on them. The README says why: the Bluetooth library (a 2015 `paypal/gatt` on a raw
  HCI channel) has no pairing layer, and "fresh pairing untested against real hardware". The Simulator transport has
  no notion of pairing at all. The emulator was written to answer G7SensorKit's client code, not from a recording of
  a real first pairing.
- **Seen:** 11-24/25, real sensor DXCMuz, our test watch, in the watch's own Bluetooth packet logs
  (`bluetoothd-hci-*.pklg` in the watch sysdiagnoses) and bluetoothd's log: 30 security requests, 24 refused in the
  background, 3 prompts unanswered (21:26:05, 23:33:59, 23:57:02), 3 accepted and bonded (22:01:13, 23:35:05,
  00:01:03). Same with the Dexcom app on the phone and in direct mode. Also on record: 10-04 (DXCMph, prompt shown
  and accepted with the app in front) and 09-18 (DXCMQj, four background pairings failed, opening the app fixed it).
  The iPhone behaved the same way on 11-24 (no alert with the app not visible; bonded when answered). Folder
  `~/Documents/LoopSysDiagnose/2026-11-24-real-g7-dexcom-pairing`.
- **Hid:** every emulator run from mid-October showed the watch's first contact succeeding from behind the clock,
  which led to wait screens that tell the user nothing is needed. G7SensorKit itself has carried the rule since
  10-23 (the "Sensor Pairing Not Finished" alert).
- **Fix:** one setting, off by default so unattended pairings in the test rig keep working (`sim.sh start -fresh`, a
  time-scale change, `/forget`, `/new` all pair with nobody there). A flag beside `-dexcom-app` (and kept by the
  `/new` re-exec), a `POST /bond` in the style of `/mute`, a control in the web page's "Display slots" card, and an
  environment variable in the harness's `sim.sh`. Levels, cheapest first:
  - *refuse*: answer `05 01 02`, `06 00`, `07 00`, hang up 0.2 s later, never serve glucose, keep the key as "not
    bonded". Reproduces the background failure on both transports; can't show success.
  - *ask*: hold the link up to 30 s for an operator's accept (API or web button), then send `08 01` and serve.
    Reproduces "someone must act within 30 s"; in the Simulator the accept could follow whether the app is in front.
  - *request* (Pi only): send a real security request and let the real watch or phone decide. Untested idea.
  - *full*: real LE Secure Connections pairing in the emulator's Bluetooth stack, with stored keys.

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
  reconnects 1–3 times a second, refused each time ("already served") (11-23 20:32). The same holds whenever no
  phone slot exists, for example a watch that pairs before any phone has. Whether a real sensor that has never
  paired advertises without pause is not established by anything we hold.

### 7. Only G7SensorKit's handshake is answered

- **Real G7:** pairs with the real Dexcom app.
- **Emulator:** answers only G7SensorKit's messages, so the real Dexcom app connects, gets the certificates and never
  finishes ("Searching for sensor", 11-23 20:09).

### 11. Pairing-tail bytes differ

- **Real G7:** verdict for a new key `05 01 02`; reply to the keep-alive `06 00`; confirmation `08 01`, sent only once
  the link is encrypted; each key-exchange round acknowledged with `0A 00 0N`.
- **Emulator:** `05 01 00`; `06 1E`; `08 00`, sent before any bond; no round acknowledgements. G7SensorKit ignores all
  four; another client might not.

### 12. No limit on displays, and a same-type newcomer replaces the earlier key

- **Real G7:** three display channels; a second display of a type already in use is refused (`05 02 02`, per
  G7SensorKit's comments and our July notes).
- **Emulator:** one key per display type with no limit on types; a newcomer of the same type silently replaces the
  earlier key. `verdictTypeRestricted` is defined and never sent.

### 13. One-minute connections are offered only to the phone

- **Real G7:** gave an unbonded watch connections about every minute (11-24 23:15–23:20 and from 23:56:55).
- **Emulator:** makes minute calls only while the phone's slot is held and the phone missed the reading.

### 14. Step timeouts are much longer

- **Real G7:** drops the link after about 4 s without a reply during the handshake.
- **Emulator:** waits 65 s at every handshake step and 6 s of quiet in a session.

### 15. A served display is refused after the link comes up

- **Real G7:** refuses a display that already has its reading at the link layer (the display sees a failed
  connection).
- **Emulator:** lets the link come up and closes it within milliseconds, which iOS reports as a connect followed by a
  disconnect. Already noted in the README.

### Suspected (not yet shown to differ from a real G7)

- **8.** In Dexcom-app mode the emulator waits 65 s for a client to turn on the authentication notifications; Loop
  never did (11-23 19:59). Whether a real G7 waits the same way is unknown.
- **9.** The phone's pairing scan never saw 6 of 12 emulated sensors (11-22), though the emulator advertised the whole
  time. Cause unknown; may be the emulator's advertising.
- **10.** "advertising full: HCI command 0x200a return 0x0C" each time the set of display types in use changes
  (11-24 17:34:13, 18:23:35). Effect on what clients see is unknown.

## Tooling (not gaps)

- `POST /new` hung once in `radio.Stop()` (11-24 15:36), and its re-exec drops `-dexcom-app` and `-interval`. Workaround: stop the
  service, move `g7state.json` aside, put fresh-sensor flags in `~/g7sim/fresh-args` (one-shot, read by `g7svc.sh`),
  start the service.
- Noted 11-23: the binary deployed on 11-07 (`4220ed2a` plus uncommitted edits) had no source on record; `0d69447` (11-07 13:33) may be it. Check before building on it.
