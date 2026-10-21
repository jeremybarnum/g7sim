package g7

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

type capture struct {
	char Char
	data []byte
}

type fakeLink struct {
	addr   string
	out    chan capture
	closed chan struct{}
	once   sync.Once
}

func newFakeLink() *fakeLink {
	return &fakeLink{addr: "AA:BB", out: make(chan capture, 64), closed: make(chan struct{})}
}

func (l *fakeLink) Addr() string { return l.addr }

func (l *fakeLink) Notify(c Char, b []byte) error {
	select {
	case l.out <- capture{c, append([]byte(nil), b...)}:
	case <-l.closed:
	}
	return nil
}

func (l *fakeLink) Close() { l.once.Do(func() { close(l.closed) }) }

func (l *fakeLink) recv(t *testing.T) capture {
	t.Helper()
	select {
	case c := <-l.out:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a notification")
		return capture{}
	}
}

func le32bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// Drives the reconnect handshake and the session the way G7SensorKit's client
// does, against the real handler and sensor model.
func TestReconnectHandshakeAndSession(t *testing.T) {
	s, act := newTestSensor(t)
	now := time.Now()
	reading := act.Add(2 * time.Hour)
	_ = reading

	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s.state.Slots[DisplayPhone] = &Slot{Key: key, LastSeen: now.Add(-time.Minute)}
	s.Tick(now) // establish the current window

	link := newFakeLink()
	h := newHandler(s, link, AdvertFull)

	h.Subscribed(CharCertificate)
	h.Subscribed(CharAuth)

	// Challenge: 02 + clientChallenge(8) + displayType.
	clientChallenge := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	msg := append([]byte{opAuthChallengeTx}, clientChallenge...)
	msg = append(msg, DisplayPhone)
	h.Write(CharAuth, msg)

	got := link.recv(t)
	if got.char != CharAuth || len(got.data) != 17 || got.data[0] != opAuthChallengeRx {
		t.Fatalf("challenge reply: %x on %s", got.data, got.char)
	}
	if !bytes.Equal(got.data[1:9], EncryptChallenge(clientChallenge, key)) {
		t.Fatalf("sensor did not answer our challenge under the key")
	}
	sensorChallenge := got.data[9:17]

	// Our answer to the sensor's challenge.
	h.Write(CharAuth, append([]byte{opAuthAnswer}, EncryptChallenge(sensorChallenge, key)...))
	got = link.recv(t)
	if got.char != CharAuth || !bytes.Equal(got.data, []byte{opAuthStatus, 0x01, 0x01}) {
		t.Fatalf("verdict: %x (want 05 01 01)", got.data)
	}

	// Session: subscribe, then ask for a reading.
	h.Subscribed(CharControl)
	h.Subscribed(CharBackfill)
	h.Write(CharControl, []byte{opGlucose})
	got = link.recv(t)
	if got.char != CharControl || len(got.data) != 19 || got.data[0] != opGlucose {
		t.Fatalf("glucose reply: %x on %s", got.data, got.char)
	}
	// The handler records it after sending the reply.
	served := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.win.servedTypes[DisplayPhone]
	}
	for deadline := time.Now().Add(time.Second); !served() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !served() {
		t.Error("the phone was not recorded as having collected the reading")
	}

	// Backfill the last hour.
	_, sensorNow := s.Latest()
	req := append([]byte{opBackfill}, le32bytes(sensorNow-3600)...)
	req = append(req, le32bytes(sensorNow)...)
	h.Write(CharControl, req)

	var frames, records int
	for {
		got = link.recv(t)
		if got.char == CharControl && got.data[0] == opBackfill {
			break
		}
		if got.char != CharBackfill {
			t.Fatalf("unexpected backfill-phase message: %x on %s", got.data, got.char)
		}
		if len(got.data)%9 != 0 {
			t.Fatalf("backfill frame length %d is not a multiple of 9", len(got.data))
		}
		frames++
		records += len(got.data) / 9
	}
	if records < 12 || frames == 0 {
		t.Fatalf("backfill delivered %d records in %d frames", records, frames)
	}

	// Versions.
	h.Write(CharControl, []byte{opExtendedVersion})
	got = link.recv(t)
	if got.data[0] != opExtendedVersion || len(got.data) < 15 {
		t.Fatalf("extended version: %x", got.data)
	}
	h.Write(CharControl, []byte{opTransmitterVer})
	got = link.recv(t)
	if got.data[0] != opTransmitterVer || len(got.data) < 20 {
		t.Fatalf("transmitter version: %x", got.data)
	}

	link.Close()
}

// A reconnect after the sensor has forgotten the slot: the sensor answers the
// challenge with noise so the client re-pairs.
func TestReconnectWithForgottenSlot(t *testing.T) {
	s, _ := newTestSensor(t)
	s.Tick(time.Now())

	link := newFakeLink()
	h := newHandler(s, link, AdvertFull)
	h.Subscribed(CharAuth)

	key := []byte("0123456789abcdef")
	clientChallenge := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	msg := append([]byte{opAuthChallengeTx}, clientChallenge...)
	msg = append(msg, DisplayPhone)
	h.Write(CharAuth, msg)

	got := link.recv(t)
	if got.data[0] != opAuthChallengeRx {
		t.Fatalf("expected a challenge reply, got %x", got.data)
	}
	if bytes.Equal(got.data[1:9], EncryptChallenge(clientChallenge, key)) {
		t.Fatal("sensor answered correctly for a key it does not have")
	}
}

// A client that only observes, as stock G7SensorKit does, gets the Dexcom app's session: the
// sensor authenticated and bonded, then the reading, then backfill and its end marker.
func TestTheDexcomAppsSessionForAnObserver(t *testing.T) {
	DexcomApp = true
	defer func() { DexcomApp = false }()
	s, _ := newTestSensor(t)
	s.Tick(time.Now())

	link := newFakeLink()
	h := newHandler(s, link, AdvertFull)
	h.Subscribed(CharAuth)
	h.Subscribed(CharControl)

	// The handler gives the client dexcomAppWait to authenticate itself first.
	var auth capture
	select {
	case auth = <-link.out:
	case <-time.After(dexcomAppWait + 2*time.Second):
		t.Fatal("no authentication status")
	}
	if auth.char != CharAuth || !bytes.Equal(auth.data, []byte{opAuthStatus, 0x01, 0x01}) {
		t.Fatalf("first notification %v %x, want authenticated and bonded", auth.char, auth.data)
	}
	glucose := link.recv(t)
	if glucose.char != CharControl || glucose.data[0] != opGlucose {
		t.Fatalf("second notification %v %x, want the reading", glucose.char, glucose.data)
	}
	h.Subscribed(CharBackfill)
	for {
		c := link.recv(t)
		if c.char == CharControl {
			if !bytes.Equal(c.data, []byte{opBackfill, 0x00}) {
				t.Fatalf("control %x, want the backfill end marker", c.data)
			}
			break
		}
		if c.char != CharBackfill {
			t.Fatalf("unexpected %v %x", c.char, c.data)
		}
	}
	select {
	case <-link.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the Dexcom app's link did not hang up")
	}
}

// A client that authenticates itself is not mistaken for an observer.
func TestAnAuthenticatingClientIsServedAsBefore(t *testing.T) {
	DexcomApp = true
	defer func() { DexcomApp = false }()
	s, _ := newTestSensor(t)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: time.Now()}
	s.Tick(time.Now())

	link := newFakeLink()
	h := newHandler(s, link, AdvertFull)
	h.Subscribed(CharAuth)
	msg := append([]byte{opAuthChallengeTx}, 1, 2, 3, 4, 5, 6, 7, 8)
	h.Write(CharAuth, append(msg, DisplayPhone))
	reply := link.recv(t)
	if reply.char != CharAuth || reply.data[0] != opAuthChallengeRx {
		t.Fatalf("reply %v %x, want the challenge answer", reply.char, reply.data)
	}
}
