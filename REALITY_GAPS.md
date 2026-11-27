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
  On the link's security channel (L2CAP channel 6) the messages are: the sensor's Security Request `0B 09` (bond,
  LE Secure Connections, no protection against a man in the middle); from the watch in the background, Pairing
  Failed `05 08` (unspecified reason); from the watch with the app in front, a Pairing Request
  `01 00 00 09 10 03 03` 3 ms after the request, answered by the sensor's Pairing Response `02 03 00 09 10 02 03`
  0.15 s later. bluetoothd logs "Displaying pairing request alert" on receiving that response, not on the Security
  Request. The watch sends its public key only when the person taps Pair (3 ms after the tap); public keys,
  confirm, random and key check follow in 0.2 s, then the link is encrypted and identity keys are exchanged.
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
- **Fixed (e1434a6), as a setting:** `-bond off|refuse|ask|request`, `POST /bond`, and a control in the web page's
  "Display slots" card. Default *off* bonds at once, so unattended pairings in the test rig keep working. *refuse*
  hangs up 0.2 s after `07 00` and serves nothing (the app not in front). *ask* holds the link for Pair from the
  API or the page (about 31 s after `06 1E`), then sends `08 01` and serves. Every new key now gets `05 01 02`; an unbonded key is kept and gets
  `05 01 02` at the next connection, and the display bonds again. "Forget bond" plays a display forgetting the
  sensor in its Bluetooth settings.
- **Level *request* (Pi only):** after `07 00` the emulator sends the sensor's real Security Request (`0B 09`) and
  the phone's or watch's own system decides, for as long as the keep-alive holds the link:
  - it answers Pairing Failed (the app not in front): the emulator logs the reason, hangs up 0.2 s later and serves
    nothing, as *refuse* does;
  - it answers with a Pairing Request (the app in front): the emulator sends the sensor's Pairing Response, on
    which the system shows its Pair alert. Nobody answers: the emulator hangs up when the keep-alive runs out.
    Cancel (the system sends Pairing Failed): it hangs up 0.2 s later. Pair: the system's public key arrives;
    the emulator cannot pair, so it declines the rest with Pairing Failed (reason 0x05, pairing not supported),
    then sends `08 01` and serves, as *ask* does after Pair. While the alert shows, Pair from the page or the API
    also counts ("Pair from here instead");
  - it never answers: the emulator hangs up when the keep-alive runs out.
  The emulator makes no key and encrypts nothing: its stack now carries the security channel's messages (they
  used to be read as attribute requests) and declines any pairing nobody asked for. Over the Simulator's socket
  there is no security channel, and *request* behaves as *ask*.
  **Differs from a real G7 at this level:** the pairing on the device ends as failed after the tap, the link is
  never encrypted, and the sensor never appears in the device's Bluetooth settings. **Seen on a device (11-27 09:07,
  Apple Watch, Loop in front, emulator log):** the watch answered the emulator's request 60 ms later with the same
  Pairing Request it sent the real sensor, sent its public key 7.7 s later (Pair tapped), and after the declined
  pairing kept the link and read glucose and backfill in the same connection. **Second run (11-27 09:12, same watch, on the clock face, its bond
  forgotten on the emulator):** the watch wrote `06 FA`, then `06 1E` and `07` 24.5 s later (21 s in the first
  run). Its system did not refuse: it sent its Pairing Request and showed the Pair alert on the clock face, 34 s
  after the reading; Pair followed 3.6 s later, then glucose. On 11-24 the same watch refused a real sensor's
  request 24 times out of 24 with the app not in front. Why it did not refuse here is a question about the watch
  (the app's state when it asked to bond), not about the request; the watch's system log is needed. **Not yet
  seen:** the refusal with the app not in front, Cancel, an unanswered alert, the iPhone.
- **Still open:** the key-discard rule (here a key never lapses), one-minute connections for an unbonded display
  (gap 13), and the level not built: *full* (real LE Secure Connections pairing in the emulator's stack, with
  stored keys and encrypted links).

### 1a. The keep-alive and a held bond request (fixed)

- **Real G7:** the display may write the keep-alive more than once and ask to bond later. Dexcom's watch app, woken
  in the background by a new sensor (11-25, DXCMuz, watch packet log), got `05 01 02`, posted its own notification
  ("G7 is ready to pair with your Watch. Tap to open app, then tap Pair."), wrote `06 FA` (answered `06 00`) and
  sent no bond request. The link stayed up with no traffic for 34 s while the app was suspended. On the tap it
  wrote `06 1E`, then `07`; the system showed the Pair prompt for the app in front, and `08 01` followed the Pair.
  Inferred, not proven: the byte after `06` is the seconds the sensor holds the link (0x1E = 30, 0xFA = 250), which
  fits the 11-24 prompts that ended about 31 s after `06 1E`.
- **Emulator before:** waited a fixed 65 s for the bond request and dropped the link at a second keep-alive.
- **Fix:** any number of keep-alives before the bond request; the link is held for the seconds the last one named
  (30 if none), plus 1 s, for the bond request and, in *ask* mode, for Pair.

### 2. Dexcom-app mode: no answer to the key-challenge nonce

- **Real G7:** answers every step of a display's pairing.
- **Emulator:** on 11-24 17:02:20 it didn't answer the watch's last pairing message (key challenge nonce); on 11-23 it
  answered in 65 ms.

### 3. A missed disconnect stops advertising for good

- **Real G7:** keeps its advertising schedule whatever a display does.
- **Emulator:** missed the watch's disconnect (11-24 17:03:11) and never advertised again, locking out the phone too.

### 4. Dexcom-app mode: the 2-second listener guess (fixed, 6a7b727)

- **Fix:** the emulator now tells an observer by what it listens to: G7SensorKit authenticating listens to the
  certificate characteristic first; observing, the authentication characteristic alone. No timing.

- **Real G7:** tells displays apart by the key each one authenticates with, not by timing.
- **Emulator:** treats a client that doesn't write within 2 s as a listener and hands it the reading. A suspended
  (slow) watch gets taken for a listener, so its pairing round 1 gets no answer (11-23 20:36:42).

### 5. Dexcom-app mode: one display's reading booked to another's slot (fixed, 6a7b727)

- **Fix:** a second observer is booked to the watch only when no client of ours holds the watch's key.

- **Real G7:** each display type collects its own reading.
- **Emulator:** booked the phone's reading to the watch's slot (display type 3, 11-24 17:34:13), then dropped the watch
  twice with "display type 3 already collected".

### 6. Dexcom-app mode: keeps advertising as unpaired after serving (fixed, 6a7b727)

- **Fix:** the simulated Dexcom app holds the slot of the display it rides (with the app's key, which no client
  of ours knows), so the sensor follows its grid. The question below, for a sensor with no display at all, stays.

- **Real G7:** goes quiet once its displays have the reading.
- **Emulator:** the simulated Dexcom app holds no slot, so the emulator keeps advertising as unpaired and the phone
  reconnects 1–3 times a second, refused each time ("already served") (11-23 20:32). The same holds whenever no
  phone slot exists, for example a watch that pairs before any phone has. Whether a real sensor that has never
  paired advertises without pause is not established by anything we hold.

### 7. Only G7SensorKit's handshake is answered

- **Real G7:** pairs with the real Dexcom app.
- **Emulator:** answers only G7SensorKit's messages, so the real Dexcom app connects, gets the certificates and never
  finishes ("Searching for sensor", 11-23 20:09).

### 11. Pairing-tail bytes differ (mostly fixed, e1434a6)

- **Fix:** `05 01 02`, `06 00` and `08 01` (only once bonded) are now sent. The `0A 00 0N` round acknowledgements
  are not, because their order against the J-PAKE payloads is not established.

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

- `POST /new` hung once in `radio.Stop()` (11-24 15:36). Workaround: stop the service, move `g7state.json` aside,
  put fresh-sensor flags in `~/g7sim/fresh-args` (one-shot, read by `g7svc.sh`), start the service. Its re-exec now
  keeps `-dexcom-app`, `-interval` and `-bond` (e1434a6).
- Noted 11-23: the binary deployed on 11-07 (`4220ed2a` plus uncommitted edits) had no source on record; `0d69447` (11-07 13:33) may be it. Check before building on it.
