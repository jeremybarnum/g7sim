package g7

import (
	"bytes"
	"encoding/binary"
	"log"
	"time"
)

// The handshake and session, as the sensor sees them. The client in
// G7SensorKit drives every exchange: it writes a request and waits for the
// reply, so this side is a straight-line state machine, one goroutine per
// link, reading the central's writes in arrival order off `in`.
//
// The wire format of each step is G7SensorKit's own
// G7Authenticator/G7Sensor; this answers what that code sends.

const (
	handshakeTimeout = 65 * time.Second // outlasts the client's own 60 s pairing deadline
	sessionIdle      = 6 * time.Second  // close once the client stops asking
	certMTU          = 20               // the certificate characteristic streams in 20-byte chunks
	dexcomAppWait    = 2 * time.Second  // a client that has written nothing by then only observes
)

// DexcomApp has a simulated Dexcom app share each link, as the Dexcom app on a phone or a
// watch does: a client that does not authenticate itself (stock G7SensorKit, which only
// observes the app's session) sees the sensor authenticated and bonded, then the reading and
// the recent backfill the app's session brings. A client that authenticates itself is served
// as before.
var DexcomApp bool

// AppDisplay is implemented by a link that knows which device's Dexcom app it rides:
// DisplayPhone or DisplayWatch. Without it, the phone's.
type AppDisplay interface {
	AppDisplay() byte
}

type inMsg struct {
	char       Char
	data       []byte
	subscribed bool
}

type handler struct {
	sensor *Sensor
	link   Link
	mode   AdvertMode

	in       chan inMsg
	done     chan struct{} // closed when the link is gone
	finished chan struct{} // closed when the protocol goroutine returns

	// Messages read ahead of what a step wanted, kept for the step that does.
	authQ   [][]byte
	ctrlQ   [][]byte
	certBuf []byte

	jpake       *JPAKE
	sessionKey  []byte // set once a J-PAKE exchange produces a key on this link
	displayType byte
	served      bool
	subscribed  map[Char]bool // the characteristics the client listens to
}

func newHandler(sensor *Sensor, link Link, mode AdvertMode) Handler {
	h := &handler{
		sensor:     sensor,
		link:       link,
		mode:       mode,
		in:         make(chan inMsg, 512),
		subscribed: map[Char]bool{},
		done:       make(chan struct{}),
		finished:   make(chan struct{}),
	}
	go func() {
		defer h.link.Close()
		defer close(h.finished)
		h.protocol()
	}()
	return h
}

func (h *handler) Write(c Char, data []byte) {
	select {
	case h.in <- inMsg{char: c, data: data}:
	case <-h.finished:
	}
}

func (h *handler) Subscribed(c Char) {
	select {
	case h.in <- inMsg{char: c, subscribed: true}:
	case <-h.finished:
	}
}

func (h *handler) Closed() { close(h.done) }

func (h *handler) protocol() {
	wait := handshakeTimeout
	if DexcomApp {
		wait = dexcomAppWait
	}
	first, ok := h.waitAuth(wait)
	if !ok && DexcomApp && !h.gone() {
		h.observed()
		return
	}
	if !ok || len(first) == 0 {
		return
	}
	switch first[0] {
	case opKeyRound: // a fresh pairing opens with the key exchange
		if !h.keyExchange(first) {
			return
		}
		challenge, ok := h.waitAuth(handshakeTimeout)
		if !ok {
			return
		}
		if !h.challenge(challenge, true) {
			return
		}
		if !h.certificatePhase() || !h.keyChallenge() || !h.finalize() {
			return
		}
	case opAuthChallengeTx: // a reconnect opens straight with the challenge
		if !h.challenge(first, false) {
			return
		}
	default:
		log.Printf("%s: unexpected opening message %x", h.link.Addr(), first)
		return
	}
	h.session()
}

// MARK: - EC-JPAKE

func (h *handler) keyExchange(first []byte) bool {
	h.jpake = NewJPAKE(h.sensor.PairingCode())
	for round := byte(0); round <= 2; round++ {
		msg := first
		if round > 0 {
			var ok bool
			if msg, ok = h.waitAuth(handshakeTimeout); !ok {
				return false
			}
		}
		if len(msg) < 2 || msg[0] != opKeyRound || msg[1] != round {
			log.Printf("%s: expected key-exchange round %d, got %x", h.link.Addr(), round, msg)
			return false
		}

		var mine []byte
		switch round {
		case 0:
			mine = h.jpake.Round1()
		case 1:
			mine = h.jpake.Round2()
		case 2:
			r3, err := h.jpake.Round3()
			if err != nil {
				log.Printf("%s: round 3: %v", h.link.Addr(), err)
				return false
			}
			mine = r3
		}
		h.notifyCert(mine)

		theirs, ok := h.waitCert(PCertLength, handshakeTimeout)
		if !ok {
			return false
		}
		switch round {
		case 0:
			h.jpake.SetClientRound(0, theirs)
		case 1:
			h.jpake.SetClientRound(1, theirs)
		case 2:
			key, err := h.jpake.SharedKey(theirs)
			if err != nil {
				log.Printf("%s: shared key: %v", h.link.Addr(), err)
				return false
			}
			h.sessionKey = key
		}
	}
	log.Printf("%s: key exchange complete", h.link.Addr())
	return true
}

// MARK: - AES challenge

func (h *handler) challenge(msg []byte, fresh bool) bool {
	if len(msg) < 10 || msg[0] != opAuthChallengeTx {
		log.Printf("%s: malformed challenge %x", h.link.Addr(), msg)
		return false
	}
	clientChallenge := msg[1:9]
	h.displayType = msg[9]
	now := time.Now()

	if !h.sensor.AdmitDisplay(h.link.Addr(), h.displayType, now, h.mode) {
		return false
	}

	var key, newKey []byte
	if fresh {
		key, newKey = h.sessionKey, h.sessionKey
	} else {
		key = h.sensor.SlotKey(h.displayType)
	}

	// Reply to the client's challenge with its answer under the key, plus our
	// own challenge. With no key (a reconnect after the slot was forgotten),
	// answer with noise: the client's own verification then fails and it
	// re-pairs with the code.
	resp := []byte{opAuthChallengeRx}
	if key != nil {
		resp = append(resp, EncryptChallenge(clientChallenge, key)...)
	} else {
		resp = append(resp, randomBytes(8)...)
	}
	sensorChallenge := randomBytes(8)
	resp = append(resp, sensorChallenge...)
	h.link.Notify(CharAuth, resp)
	if key == nil {
		log.Printf("%s: no key for display type %d; client will re-pair", h.link.Addr(), h.displayType)
		return false
	}

	answer, ok := h.waitAuth(handshakeTimeout)
	if !ok {
		return false
	}
	if len(answer) < 9 || answer[0] != opAuthAnswer {
		return false
	}
	if !bytes.Equal(answer[1:9], EncryptChallenge(sensorChallenge, key)) {
		v := h.sensor.Rejected(verdictMismatch)
		h.link.Notify(CharAuth, []byte{opAuthStatus, v.Status, v.Detail})
		log.Printf("%s: challenge answer did not match", h.link.Addr())
		return false
	}

	v := h.sensor.Authenticated(h.link.Addr(), h.displayType, newKey, now)
	h.link.Notify(CharAuth, []byte{opAuthStatus, v.Status, v.Detail})
	if v.Status != 0x01 {
		log.Printf("%s: refused (status %02x detail %02x)", h.link.Addr(), v.Status, v.Detail)
		return false
	}
	log.Printf("%s: authenticated as display type %d", h.link.Addr(), h.displayType)
	return true
}

// MARK: - Certificate exchange (fresh pairing only)

func (h *handler) certificatePhase() bool {
	chain := h.sensor.CertChain()
	for {
		msg, ok := h.waitAuth(handshakeTimeout)
		if !ok {
			return false
		}
		if len(msg) < 6 || msg[0] != opCertificate {
			log.Printf("%s: expected a certificate request, got %x", h.link.Addr(), msg)
			return false
		}
		index := int(msg[1])
		clientCertLen := int(binary.LittleEndian.Uint32(msg[2:6]))

		// A zero-length request is the terminator: acknowledge and stop.
		if clientCertLen == 0 {
			h.link.Notify(CharAuth, []byte{opCertificate, 0x00, byte(index), 0x00, 0x00})
			return true
		}

		// Our certificate at this index — the dummy CA at 0, the leaf at 1, as
		// a real sensor sends them. The acknowledgement declares its length as
		// a 4-byte little-endian value (the real sensor's 7-byte 0B ack).
		var cert []byte
		if index < len(chain) {
			cert = chain[index]
		}
		ack := le32([]byte{opCertificate, 0x00, byte(index)}, uint32(len(cert)))
		h.link.Notify(CharAuth, ack)
		h.notifyCert(cert)

		if _, ok := h.waitCert(clientCertLen, handshakeTimeout); !ok {
			return false
		}
	}
}

// MARK: - Key challenge (fresh pairing only)

func (h *handler) keyChallenge() bool {
	msg, ok := h.waitAuth(handshakeTimeout)
	if !ok {
		return false
	}
	if len(msg) < 17 || msg[0] != opKeyChallenge {
		log.Printf("%s: expected the key challenge, got %x", h.link.Addr(), msg)
		return false
	}
	displayNonce := msg[1:17]

	// Our challenge to the display: the 16 bytes it must sign with Dexcom's
	// display key. The ack is `0C 00 <16 bytes>`, which the client signs.
	sensorChallenge := randomBytes(16)
	h.link.Notify(CharAuth, append([]byte{opKeyChallenge, 0x00}, sensorChallenge...))

	// Sign the display's nonce with the sensor leaf key, as a real sensor does.
	h.notifyCert(h.sensor.SignChallenge(displayNonce))

	// The display answers with its signature over our challenge. Verify it
	// proves possession of Dexcom's display key — the check a real sensor
	// makes. Advisory for now (logged, not enforced) so a verification quirk
	// cannot block pairing.
	displaySig, ok := h.waitCert(64, handshakeTimeout)
	if !ok {
		return false
	}
	if VerifyDisplaySignature(sensorChallenge, displaySig) {
		log.Printf("%s: display authenticated — holds Dexcom's display key", h.link.Addr())
	} else {
		log.Printf("%s: display's key-challenge signature did not verify against Dexcom's display key (advisory)", h.link.Addr())
	}
	return true
}

// MARK: - Finalize

func (h *handler) finalize() bool {
	msg, ok := h.waitAuth(handshakeTimeout)
	if !ok || len(msg) == 0 || msg[0] != opFinalize {
		return false
	}
	h.link.Notify(CharAuth, []byte{opFinalize, 0x1E})

	msg, ok = h.waitAuth(handshakeTimeout)
	if !ok || len(msg) == 0 || msg[0] != opBondRequest {
		return false
	}
	h.link.Notify(CharAuth, []byte{opBondRequest, 0x00})
	// The sensor confirms the bond; the client treats this as optional.
	h.link.Notify(CharAuth, []byte{opBondConfirmed, 0x00})
	log.Printf("%s: paired", h.link.Addr())
	return true
}

// MARK: - Session

func (h *handler) session() {
	for {
		msg, ok := h.waitControl(sessionIdle)
		if !ok {
			return
		}
		if len(msg) == 0 {
			continue
		}
		h.handleControl(msg)
	}
}

func (h *handler) handleControl(msg []byte) {
	now := time.Now()
	switch msg[0] {
	case opGlucose:
		r, sensorNow := h.sensor.Latest()
		if r == nil {
			return
		}
		h.link.Notify(CharControl, GlucoseMessage(r, sensorNow))
		h.sensor.Served(h.link.Addr(), h.displayType, now)
		h.served = true

	case opBackfill:
		if len(msg) >= 9 {
			start := binary.LittleEndian.Uint32(msg[1:5])
			end := binary.LittleEndian.Uint32(msg[5:9])
			records := h.sensor.Backfill(start, end)
			// Two 9-byte records per notification, as a G7 packs them.
			for i := 0; i < len(records); i += 2 {
				frame := BackfillRecord(records[i])
				if i+1 < len(records) {
					frame = append(frame, BackfillRecord(records[i+1])...)
				}
				h.link.Notify(CharBackfill, frame)
			}
			log.Printf("%s: backfilled %d readings (%d..%d)", h.link.Addr(), len(records), start, end)
		}
		// The acknowledgement doubles as the end-of-stream marker.
		h.link.Notify(CharControl, []byte{opBackfill, 0x00})

	case opExtendedVersion:
		h.link.Notify(CharControl, h.sensor.ExtendedVersion())

	case opTransmitterVer:
		h.link.Notify(CharControl, TransmitterVersionMessage(h.sensor.Serial()))

	case opCalibrate:
		if len(msg) >= 7 {
			glucose := binary.LittleEndian.Uint16(msg[1:3])
			age := binary.LittleEndian.Uint32(msg[3:7])
			h.sensor.Calibrate(glucose, age, h.displayType)
			h.link.Notify(CharControl, []byte{opCalibrate, 0x00, 0x01, 0x00}) // accepted
			log.Printf("%s: calibrated to %d mg/dL at sensor age %ds", h.link.Addr(), glucose, age)
		}

	case opCalibrationBounds:
		h.link.Notify(CharControl, h.sensor.CalibrationBounds())

	case opDisconnect:
		log.Printf("%s: client asked to disconnect", h.link.Addr())

	default:
		log.Printf("%s: unhandled control opcode %02x", h.link.Addr(), msg[0])
	}
}

// MARK: - The Dexcom app's session, for a client that only observes

// observed plays the Dexcom app's session on the link: its authentication shows as
// authenticated and bonded, then its reading and the last hour's backfill arrive, and the app
// hangs up.
func (h *handler) observed() {
	now := time.Now()
	if a, ok := h.link.(AppDisplay); ok {
		h.displayType = a.AppDisplay()
	} else {
		// Over real Bluetooth a link cannot say which device it is on: the first observer in a
		// window is the phone's Dexcom app, another one the watch's.
		h.displayType = h.sensor.ObserverDisplay(h.link.Addr())
	}
	if !h.sensor.AdmitDisplay(h.link.Addr(), h.displayType, now, h.mode) {
		return
	}
	if !h.waitSubscribed(CharAuth, handshakeTimeout) {
		return
	}
	h.link.Notify(CharAuth, []byte{opAuthStatus, 0x01, 0x01})
	if !h.waitSubscribed(CharControl, 10*time.Second) {
		return
	}
	r, sensorNow := h.sensor.Latest()
	if r == nil {
		return
	}
	h.link.Notify(CharControl, GlucoseMessage(r, sensorNow))
	h.sensor.Served(h.link.Addr(), h.displayType, now)
	h.served = true
	log.Printf("%s: served reading at %d as the Dexcom app's session (display type %d)", h.link.Addr(), r.Time, h.displayType)
	// The client listens to backfill once it has the reading.
	if h.waitSubscribed(CharBackfill, 5*time.Second) {
		start := uint32(0)
		if sensorNow > 3600 {
			start = sensorNow - 3600
		}
		records := h.sensor.Backfill(start, sensorNow)
		for i := 0; i < len(records); i += 2 {
			frame := BackfillRecord(records[i])
			if i+1 < len(records) {
				frame = append(frame, BackfillRecord(records[i+1])...)
			}
			h.link.Notify(CharBackfill, frame)
		}
		h.link.Notify(CharControl, []byte{opBackfill, 0x00})
	}
	// The app's link stays up a few seconds after its session, then it hangs up.
	h.readNext(3 * time.Second)
}

// waitSubscribed waits for the client to listen to a characteristic. Writes that arrive
// meanwhile are kept for the steps that want them.
func (h *handler) waitSubscribed(c Char, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !h.subscribed[c] {
		m, ok := h.readNext(time.Until(deadline))
		if !ok {
			return false
		}
		switch {
		case m.subscribed:
		case m.char == CharAuth:
			h.authQ = append(h.authQ, m.data)
		case m.char == CharControl:
			h.ctrlQ = append(h.ctrlQ, m.data)
		case m.char == CharCertificate:
			h.certBuf = append(h.certBuf, m.data...)
		}
	}
	return true
}

func (h *handler) gone() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// MARK: - Reading the central's writes

func (h *handler) readNext(timeout time.Duration) (inMsg, bool) {
	if timeout < 0 {
		timeout = 0
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case m := <-h.in:
		if m.subscribed {
			h.subscribed[m.char] = true
		}
		return m, true
	case <-t.C:
		return inMsg{}, false
	case <-h.done:
		return inMsg{}, false
	}
}

func (h *handler) waitAuth(timeout time.Duration) ([]byte, bool) {
	if len(h.authQ) > 0 {
		m := h.authQ[0]
		h.authQ = h.authQ[1:]
		return m, true
	}
	deadline := time.Now().Add(timeout)
	for {
		m, ok := h.readNext(time.Until(deadline))
		if !ok {
			return nil, false
		}
		switch {
		case m.subscribed:
		case m.char == CharAuth:
			return m.data, true
		case m.char == CharCertificate:
			h.certBuf = append(h.certBuf, m.data...)
		case m.char == CharControl:
			h.ctrlQ = append(h.ctrlQ, m.data)
		}
	}
}

func (h *handler) waitControl(timeout time.Duration) ([]byte, bool) {
	if len(h.ctrlQ) > 0 {
		m := h.ctrlQ[0]
		h.ctrlQ = h.ctrlQ[1:]
		return m, true
	}
	deadline := time.Now().Add(timeout)
	for {
		m, ok := h.readNext(time.Until(deadline))
		if !ok {
			return nil, false
		}
		switch {
		case m.subscribed:
		case m.char == CharControl:
			return m.data, true
		case m.char == CharAuth:
			h.authQ = append(h.authQ, m.data)
		case m.char == CharCertificate:
			h.certBuf = append(h.certBuf, m.data...)
		}
	}
}

func (h *handler) waitCert(n int, timeout time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	for len(h.certBuf) < n {
		m, ok := h.readNext(time.Until(deadline))
		if !ok {
			return nil, false
		}
		switch {
		case m.subscribed:
		case m.char == CharCertificate:
			h.certBuf = append(h.certBuf, m.data...)
		case m.char == CharAuth:
			h.authQ = append(h.authQ, m.data)
		case m.char == CharControl:
			h.ctrlQ = append(h.ctrlQ, m.data)
		}
	}
	out := make([]byte, n)
	copy(out, h.certBuf)
	h.certBuf = append([]byte(nil), h.certBuf[n:]...)
	return out, true
}

// notifyCert streams a payload on the certificate characteristic in the
// 20-byte chunks the client reassembles by byte count.
func (h *handler) notifyCert(data []byte) {
	for i := 0; i < len(data); i += certMTU {
		end := i + certMTU
		if end > len(data) {
			end = len(data)
		}
		if err := h.link.Notify(CharCertificate, data[i:end]); err != nil {
			return
		}
	}
}
