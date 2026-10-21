# g7sim

A Dexcom G7 sensor emulator, for exercising G7SensorKit's direct connection without a
real sensor. It has two transports:

- **Bluetooth** (`-transport ble`, the default): a real radio on Linux, such as a
  Raspberry Pi, for an app on a phone. Built on the same `paypal/gatt` HCI stack as
  [LoopKit/pod](https://github.com/LoopKit/pod).
- **iOS Simulator** (`-transport impossible`): on a Mac, g7sim stands in for Bluetooth
  for an app in the Simulator, through [ImpossiBLE](https://github.com/mickeyl/ImpossiBLE).
  No radio needed.

> **For development use only — not a Dexcom product.** g7sim is an independent,
> unofficial tool for testing CGM software. It is not made, endorsed, or supported by
> Dexcom, Inc., it is not a medical device, and it emits only simulated, made-up data.
> "Dexcom", "G7", "ONE+" and "Stelo" are trademarks of Dexcom, Inc., used here solely to
> describe interoperability. Never use it for therapy or any clinical decision.

## Status

| Part | State |
|---|---|
| Advertising schedule (bursts, tails, minute calls, lockout) | done, unit tested, runs on the Pi |
| Display slots, served-client refusal, types-in-use byte | done, unit tested |
| Readings: warmup, expiry, sine/fixed/walk glucose, calibration offset, 24 h backfill store | done, unit tested |
| Reply encoders (4e, backfill record, 52, 4a, 32) | done |
| Key-exchange and challenge crypto (`g7/jpake.go`) | done, tested against a port of the app's client and the app's AES vectors |
| GATT layout, random static address, HTTP API | done, runs on the Pi |
| Per-connection protocol handler (`g7/handler.go`) | done, reconnect + session integration-tested; fresh pairing untested against real hardware |
| iOS Simulator transport (`impossible.go`, via ImpossiBLE) | done; Loop pairs, reads and reconnects in the Simulator |
| Wake-path check, suspension, termination relaunch + state restoration | done, unit tested; run against Loop |

## Build and run on a Raspberry Pi

Go 1.15 (what the Pi has) is enough. From a checkout on the Pi:

```bash
go test ./g7/
go build -o g7sim .
sudo setcap 'cap_net_raw,cap_net_admin=eip' ./g7sim
./g7sim -fresh                # new sensor, activated 2 h ago, random code and serial
./g7sim                       # restore the sensor in g7state.json
./g7sim -fresh -age 0         # new sensor in warmup
./g7sim -fresh -code 1234 -serial 123456789012 -days 15
./g7sim -fresh -model stelo   # a Stelo (DX01xx); also oneplus (DX02xx), default g7 (DXCMxx)
./g7sim -fresh -age 0 -lifetime 36h -warmup 5m   # short session, to exercise warmup and expiry
```

`-model` sets the advertised name prefix, the only thing G7SensorKit uses to tell the models
apart. `-days` picks the 10- or 15-day preset (15 for Stelo, 10 otherwise): the session length,
warmup, algorithm version and maximum lifetime that a real sensor of that kind reports in its
version reply. `-lifetime` (before the 12-hour grace period) and `-warmup` override the preset's
lengths. All of these are fixed when a sensor is created; to change them, start a new sensor
with `-fresh` or from the web UI.

The emulator takes over `hci0`, so it cannot run beside the pod simulator on one adapter;
use a USB dongle for the second one. The sensor advertises from a random static address
derived from its serial, so each `-fresh` sensor is a new peripheral to iOS.

### bluetoothd conflicts for the adapter

The emulator drives `hci0` directly over a raw HCI socket, so the system Bluetooth daemon must
not be holding the adapter. On older Pi OS (e.g. Raspbian bullseye on a Pi 3/4) it coexists
fine, but on newer releases (Raspbian trixie, e.g. a Pi Zero W) `bluetoothd` keeps the adapter
and the emulator fails at start-up with `starting Bluetooth: no supported devices available`.
Stop (or disable) the daemon first:

```bash
sudo systemctl stop bluetooth      # or: sudo systemctl disable --now bluetooth
./g7sim -fresh                     # as root, or with the setcap above
```

A tidy way to run it as a dedicated appliance is a unit with `Conflicts=bluetooth.service`
(systemd stops `bluetooth` whenever the emulator starts). The Pi Zero W has no RTC, so make
sure its clock is NTP-synced before running — the emulator timestamps readings at wall-clock
time, and a phone will reject readings from a sensor whose clock is off.

### No system Bluetooth bond

Unlike a real G7, the emulator does **not** appear under Settings → Bluetooth on the phone,
and pairing creates no system-level bond. The raw-HCI stack it uses has no Security Manager
(SMP) layer: it serves ATT only and never answers a pairing request, so iOS connects without
encrypting the link. This is harmless — the G7 direct connection authenticates entirely at
the application layer (the 4-digit-code J-PAKE handshake and the AES reconnect challenge), so
readings flow normally. The one visible difference is that the sensor is absent from the
phone's Bluetooth list, and the app's "forget the sensor under Settings → Bluetooth" step is
a no-op for it.

### Web UI

Open `http://<pi>:8080/` for a control panel: the current reading and state, the
glucose model (fixed / sine / random walk), a dropdown to make the sensor report any
algorithm state or error, the display slots, and a button to forget all keys. **Start a new
sensor** replaces the sensor with one built from the form (model, preset, lifetime, warmup,
activation age, code, serial; blanks pick defaults): the emulator saves it over the state file
and re-executes itself, so the new sensor comes up exactly as from a fresh launch, with its own
name and address. A phone paired to the old sensor must pair the new one. The page is
self-contained and polls the JSON API below, so it needs no internet on the Pi.

![The web control panel](docs/screenshot.png)

### Control API (`-api`, default `:8080`)

```bash
curl -s rpi:8080/status
curl -s -XPOST rpi:8080/glucose -d '{"kind":"fixed","value":65}'
curl -s -XPOST rpi:8080/glucose -d '{"kind":"sine","value":120,"amplitude":50,"periodMinutes":240}'
curl -s -XPOST rpi:8080/glucose -d '{"kind":"walk","value":140}'
curl -s -XPOST rpi:8080/glucose -d '{"kind":"fixed","value":120,"algorithmState":18}'
curl -s -XPOST rpi:8080/forget          # drop every display's key
curl -s -XPOST rpi:8080/new -d '{"model":"stelo","code":"1234","ageMinutes":0}'   # new sensor, then restart
```

`/new` takes `model`, `days`, `code`, `serial`, `ageMinutes`, `lifetimeHours` and
`warmupMinutes`; anything omitted picks the same default as the command line.

## Run it for Loop in the iOS Simulator

CoreBluetooth does nothing in the iOS Simulator. [ImpossiBLE](https://github.com/mickeyl/ImpossiBLE)
replaces it inside the app with shims that speak JSON over the Unix socket `/tmp/impossible.sock`,
and `-transport impossible` makes g7sim the provider on that socket: no radio, no Pi, no
Bluetooth permission, so it runs on any Mac, including a CI virtual machine.

```bash
go build -o g7sim .
./g7sim -transport impossible -fresh -code 4321 -serial 7810293746
simulator/build-impossible.sh        # prints the paths of libImpossiBLE.dylib and libImpossiBLE-watch.dylib
```

Loop needs no change to use it: ImpossiBLE is injected at launch.

```bash
SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=/path/to/libImpossiBLE.dylib \
    xcrun simctl launch <simulator> <Loop's bundle id>
```

From Xcode, set `DYLD_INSERT_LIBRARIES` to the same path under the Loop scheme's Run →
Arguments → Environment Variables (a local change; don't commit it). Without the library Loop
behaves as before in the Simulator: pairing walks through with a stand-in sensor.

Code `4321` and serial `7810293746` are the stand-in applicator G7SensorKit offers in the
Simulator, so both "scan" and manual code entry find this sensor. ImpossiBLE's menu bar app
serves the same socket, so quit it first; g7sim refuses to start while another provider is
live. A provider serves one app at a time, and a newly launched app takes over.

The app sees the sensor's advertising schedule, not a sensor that is always there: it is
discovered only while it advertises, and a connect waits for the next advert, as iOS's does.
The protocol, the web UI and the API are the same as on the Pi.

### With other simulated devices

ImpossiBLE connects the app to one socket, so one provider. `impossible-mux` holds that
socket and connects the app to several providers on their own, such as g7sim and the
[LoopKit/pod](https://github.com/LoopKit/pod) simulator, so Loop runs with both a simulated
sensor and a simulated pod:

```bash
go build -o impossible-mux ./cmd/impossible-mux
./g7sim -transport impossible -socket /tmp/impossible-g7.sock
pod -transport impossible -socket /tmp/impossible-pod.sock
./impossible-mux /tmp/impossible-g7.sock /tmp/impossible-pod.sock
```

Each provider sees the app as its only client. The mux sends the app's session-wide messages
(hello, scans) to all of them and each peripheral's requests to the provider that has it; a
connect to a peripheral it has not seen (one the app retrieved after a relaunch) goes to all,
and the one that holds it owns it. A provider that restarts rejoins, and its peripherals
disconnect from the app meanwhile, as a device that powers off does.

Several apps can share the mux, such as Loop's phone app and its watch app in a paired pair of
Simulators (inject `libImpossiBLE-watch.dylib` into the watch app). Each provider still sees one
client. A peripheral has one link, as over the air: the app that holds it talks to it, another
app's connect waits until the holder lets go, and its other requests are answered as on a
disconnected peripheral. Each connect names its app, so g7sim treats each app as its own
display, with its own share of every reading.

With `-api :8087` an app or a provider can be taken out of range and brought back, for tests
that need a phone away from its pod or a pod out of reach:

```bash
curl -X POST 'localhost:8087/range?app=<bundle id>&in=0'   # the app out of range
curl -X POST 'localhost:8087/range?provider=pod&in=0'      # the pod out of range of every app
curl 'localhost:8087/range'                                 # what is out of range
```

A link that goes out of range drops with a timeout, a connect waits, and the peripheral's last
advertisement is repeated when the app is back. `MUX_TRACE=1` logs each request and event for a
peripheral with the app that holds it.

For [suspension](#background-wakes-suspension-and-termination), give `-suspend` to the mux
rather than to g7sim, so every provider's messages wake the app:

```bash
./impossible-mux -simulator <simulator> -suspend /tmp/impossible-g7.sock /tmp/impossible-pod.sock
```

### Background wakes, suspension and termination

On a phone a backgrounded Loop runs only when iOS wakes it for a Bluetooth request it is
holding: a pending connect, a scan, or a notification on a live link. g7sim checks this at
every reading. If the app has no pending connect, no scan and no link when the sensor starts
advertising, it logs `NO WAKE PATH` with a running count: on a phone, Loop would sleep through
that reading.

The Simulator does not enforce this by itself. runningboard decides when to suspend a
backgrounded app, as on a phone, but cannot stop a Mac process, so the app keeps running. With
`-suspend`, g7sim follows runningboard's verdicts from the simulator's log and stops the app
(`SIGSTOP`) while it is suspended. Everything g7sim sends a suspended app is a Bluetooth event
that iOS would wake it for, so each one gives the app 10 s of run time (`SIGCONT`), as
bluetoothd's assertion does. A reading the app had nothing lodged for is then really missed.

```bash
./g7sim -transport impossible -simulator <simulator> -suspend
```

g7sim learns runningboard's verdicts from changes, so it treats an app it has just met as
running until the next one. When g7sim exits on a signal it resumes the app; a `kill -9` of
g7sim leaves it stopped (`kill -CONT <pid>`).

When the app ends with a connect or scan lodged (`kill -9`, as iOS's memory pressure does),
g7sim holds the requests, as bluetoothd does. At the next reading it relaunches the app with
ImpossiBLE injected, and hands the requests back at the app's first call: the app gets
`centralManager(_:willRestoreState:)` and the held connect completes.

```bash
./g7sim -transport impossible -simulator <simulator> -relaunch   # injects simulator/build/libImpossiBLE.dylib
kill -9 <Loop's pid>                                      # iOS terminates Loop
curl -X POST localhost:8080/force-quit                   # or: the user swiped it away; nothing relaunches it
```

Without `-relaunch`, g7sim only logs when iOS would relaunch the app. Where it differs from iOS:
- simctl launches the app in the foreground; iOS relaunches it in the background.
- The restore arrives after the app's first call, so after `centralManagerDidUpdateState`; on
  iOS it comes first.
- The restored peripheral is still connecting, and the connect completes once the app is
  back. On iOS, bluetoothd has already connected when the app relaunches.
- A link that is up when the app ends is closed, not kept.

`build-impossible.sh` builds a pinned ImpossiBLE release with
`simulator/impossible-delegate-queue.patch`, which fixes a state callback delivered on the main
queue rather than the central's own (G7SensorKit asserts its queue, so Loop crashed at launch).

## The advertising model

From Jeremy Barnum's sniffer and scanner measurements of a real G7 (Loop
`docs/G7_MUTE_INVESTIGATION_2026-09.md`, in git history at `b930fe17`). All values are
in `g7.MeasuredTiming()`.

- A reading every 300 s from activation; the burst starts 2.5 s after it, adverts 160 ms apart.
- Every held display collected: the burst ends at start + 7 s, or 1 s after the last link closes.
- A held display missing, in its first two missed windows: start + 29 s, or 19 s after the last close.
- Missing for longer: start + 8 s. Nobody connected at all: 25 s.
- While the phone's slot is held and it missed the current reading: bare adverts
  (Flags `0x04` only) for 3 s at +60/120/180/240 s, admitting only the phone.
- A display slot stays held 15 min after it was last seen; held slots set the
  `1 << (type-1)` bits of the advertisement's types-in-use byte.
- A client that collected this window is refused for the rest of it: by address the
  moment its link comes up, and by display type at its challenge.
- Four rejections in a row stop all advertising for 10 min.

What the emulator cannot copy: the real sensor refuses a served client at the link
layer (the central sees connection-failed-to-be-established, 0x3E). Here the link comes
up and is closed within milliseconds, which iOS reports as a connect followed by a
disconnect.

## The protocol handler

`g7/handler.go` is the sensor's side of one connection. The wire protocol it answers is
the client in G7SensorKit — the sequence, framing and byte layouts come from
`G7SensorKit/G7CGMManager/G7Authenticator.swift` (pairing and reconnect handshake),
`G7Sensor.swift` (`beginSession`, `requestBackfillIfNeeded`, `sendPendingCalibration`,
`bluetoothManager(_:didReceiveControlResponse:)`), and the `Messages/` parsers. The
emulator is right when that code, unmodified, pairs, reconnects and reads through it.

### Shape

The BLE layer (`ble.go`) gives the handler one goroutine per link and delivers the
central's writes and subscriptions through the `g7.Handler` interface in arrival order:

```go
type Handler interface {
	Write(c Char, data []byte) // a central's write
	Subscribed(c Char)         // a central enabled notifications
	Closed()                   // the link is gone
}
```

`newHandler` spawns a goroutine that reads those off a channel and runs the exchange
straight-line (`protocol()`), the same way the app's `G7Authenticator.run()` reads as a
sequence rather than a callback web. It replies with `link.Notify(char, bytes)` and ends
the link with `link.Close()` (deferred, so any early return hangs up). Certificate-stream
reads reassemble by byte count (`waitCert`); auth and control reads take one message at a
time (`waitAuth`, `waitControl`), stashing anything that arrives out of turn.

### The two openings

The first write on the authentication characteristic tells the handler which path the
client is on:

- `0A 00` — a fresh pairing. `keyExchange` runs the three J-PAKE rounds (streaming the
  sensor's cert and reading the client's on each), deriving the session key. Then the
  AES `challenge`, then `certificatePhase` (answering each `0B` with a block of the
  length it declares, which the client reads but does not verify), `keyChallenge` (`0C`),
  and `finalize` (`06`/`07`/`08`).
- `02 …` — a reconnect. Straight to `challenge` with the slot's stored key.

`challenge` answers the client's challenge under the key and poses its own; a wrong answer
is a `05 02 01` rejection. With no stored key (the slot was forgotten) it replies with
noise, so the client's own verification fails and it re-pairs with the code. On success,
`session()` answers the control opcodes: `4e` glucose, `59` backfill, `52`/`4a` versions,
`34`/`32` calibration. It hangs up after `sessionIdle` of quiet.

### A client that only observes (`-dexcom-app`)

Stock G7SensorKit never authenticates: on a phone or a watch it relies on the Dexcom app,
whose link it shares, and only listens. With `-dexcom-app`, a simulated Dexcom app shares
each link. A client that writes nothing on the authentication characteristic within two
seconds sees `05 01 01` (authenticated and bonded) once it listens there, then the reading
once it listens to control, then the last hour's backfill and its `59 00` end marker; the
app then hangs up. A client that authenticates itself is served as above. In the Simulator
transport, the app also connects at each reading: an app that registered for connection
events (G7SensorKit's ride-only mode, which joins the Dexcom app's link instead of
connecting itself) hears a connection event, with the sensor's name, and joins.

### What the sensor model provides

| Need | Call |
|---|---|
| Key exchange over the pairing code | `NewJPAKE(sensor.PairingCode())` |
| Challenge transform | `EncryptChallenge(challenge, key)` |
| May this display connect? (served-this-window, minute-call rules) | `sensor.AdmitDisplay(addr, type, now, mode)` |
| Stored key for a display type | `sensor.SlotKey(type)` |
| Record a proven key; returns the verdict to send | `sensor.Authenticated(addr, type, newKeyOrNil, now)` |
| Record a failed proof (counts toward the lockout) | `sensor.Rejected(verdictMismatch)` |
| Mark a client as having collected the reading | `sensor.Served(addr, type, now)` |
| Latest reading and the sensor clock | `sensor.Latest()` → `GlucoseMessage(r, now)` |
| Readings in a range | `sensor.Backfill(start, end)` → `BackfillRecord(r)` |
| Calibration | `sensor.Calibrate(glucose, at, type)`, `sensor.CalibrationBounds()` |
| Version replies | `sensor.ExtendedVersion()`, `TransmitterVersionMessage(sensor.Serial())` |

### Testing it

`handler_test.go` drives the reconnect handshake and the session (challenge → glucose →
backfill → versions) against the real handler and sensor model through an in-memory link.
The fresh-pairing path is exercised by `jpake_test.go` at the crypto layer but has not yet
been run end to end against the app; the first real check is to pair from Loop:

    ./g7sim -fresh -code 1234

then read the emulator log beside Loop's device communication log — the two show the same
exchange from both ends.

## Credits and license

- The advertising timing model is from Jeremy Barnum's sniffer and scanner measurements of
  a real G7 (September 2026).
- The protocol layouts, display-type values and advertisement format are as implemented in
  [LoopKit/G7SensorKit](https://github.com/LoopKit/G7SensorKit), which in turn draws on
  [DexKit](https://github.com/nightscout/DexKit) by Erik Tolboom.
- The iOS Simulator transport serves the socket protocol of Mickey Lauer's
  [ImpossiBLE](https://github.com/mickeyl/ImpossiBLE) (MIT), which g7sim does not vendor.
- The BLE approach follows [LoopKit/pod](https://github.com/LoopKit/pod); the HCI stack is a
  vendored copy of [paypal/gatt](https://github.com/paypal/gatt) under `third_party/gatt`,
  with small fixes noted in the README's library-changes notes.

g7sim is MIT licensed (see `LICENSE`). The vendored `paypal/gatt` keeps its own BSD license
in `third_party/gatt/LICENSE.md`.
