package g7

import (
	"bytes"
	"testing"
	"time"
)

// bareHandler is a handler with no protocol goroutine, so a test can drive one step.
func bareHandler(s *Sensor, link *fakeLink, displayType byte) *handler {
	return &handler{
		sensor: s, link: link, mode: AdvertFull,
		in: make(chan inMsg, 64), done: make(chan struct{}), finished: make(chan struct{}),
		subscribed: map[Char]bool{}, displayType: displayType,
	}
}

func expectNotify(t *testing.T, link *fakeLink, char Char, want []byte) {
	t.Helper()
	got := link.recv(t)
	if got.char != char || !bytes.Equal(got.data, want) {
		t.Fatalf("got %x on %s, want %x on %s", got.data, got.char, want, char)
	}
}

func expectSilence(t *testing.T, link *fakeLink, d time.Duration) {
	t.Helper()
	select {
	case c := <-link.out:
		t.Fatalf("unexpected %x on %s", c.data, c.char)
	case <-time.After(d):
	}
}

// finalizeWith runs the finalize step against the given bond mode with a new, unbonded watch key.
func finalizeWith(t *testing.T, mode BondMode) (*Sensor, *fakeLink, *handler, chan bool) {
	s, _ := newTestSensor(t)
	s.SetBondMode(mode)
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: time.Now(), Unbonded: true}
	link := newFakeLink()
	h := bareHandler(s, link, DisplayWatch)
	result := make(chan bool, 1)
	go func() { result <- h.finalize() }()
	h.in <- inMsg{char: CharAuth, data: []byte{opFinalize, 0x1E}}
	expectNotify(t, link, CharAuth, []byte{opFinalize, 0x00})
	h.in <- inMsg{char: CharAuth, data: []byte{opBondRequest}}
	expectNotify(t, link, CharAuth, []byte{opBondRequest, 0x00})
	return s, link, h, result
}

func bonded(s *Sensor, t byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.state.Slots[t]
	return slot != nil && !slot.Unbonded
}

func TestBondOffConfirmsAtOnce(t *testing.T) {
	s, link, _, result := finalizeWith(t, BondInstant)
	expectNotify(t, link, CharAuth, []byte{opBondConfirmed, 0x01})
	if !<-result || !bonded(s, DisplayWatch) {
		t.Fatal("not bonded")
	}
}

func TestBondRefusedHangsUpWithoutConfirming(t *testing.T) {
	s, link, _, result := finalizeWith(t, BondRefuse)
	select {
	case ok := <-result:
		if ok {
			t.Fatal("refused bond reported as bonded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the sensor did not hang up")
	}
	expectSilence(t, link, 50*time.Millisecond)
	if bonded(s, DisplayWatch) {
		t.Fatal("slot marked bonded")
	}
	if s.SlotKey(DisplayWatch) == nil {
		t.Fatal("the key was dropped; a real G7 keeps it")
	}
}

func TestBondAskWaitsForPair(t *testing.T) {
	s, link, _, result := finalizeWith(t, BondAsk)
	expectSilence(t, link, 100*time.Millisecond)
	if got := s.Status()["bondPending"].([]map[string]interface{}); len(got) != 1 || got[0]["displayType"] != int(DisplayWatch) {
		t.Fatalf("pending %v, want the watch's prompt", got)
	}
	if got := s.AcceptBond(0); len(got) != 1 || got[0] != int(DisplayWatch) {
		t.Fatalf("accepted %v", got)
	}
	expectNotify(t, link, CharAuth, []byte{opBondConfirmed, 0x01})
	if !<-result || !bonded(s, DisplayWatch) {
		t.Fatal("not bonded after Pair")
	}
	if got := s.Status()["bondPending"].([]map[string]interface{}); len(got) != 0 {
		t.Fatalf("prompt still pending: %v", got)
	}
}

func TestBondAskUnansweredHangsUp(t *testing.T) {
	saved := bondPromptLimit
	bondPromptLimit = 200 * time.Millisecond
	defer func() { bondPromptLimit = saved }()
	s, link, _, result := finalizeWith(t, BondAsk)
	select {
	case ok := <-result:
		if ok {
			t.Fatal("unanswered prompt reported as bonded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the sensor did not hang up")
	}
	expectSilence(t, link, 50*time.Millisecond)
	if bonded(s, DisplayWatch) || len(s.AcceptBond(0)) != 0 {
		t.Fatal("bonded, or the prompt outlived the link")
	}
}

// A new key gets 05 01 02; a forgotten bond turns a bonded key back into 05 01 02.
func TestVerdictsFollowTheBond(t *testing.T) {
	s, _ := newTestSensor(t)
	now := time.Now()
	key := make([]byte, 16)
	if v := s.Authenticated("A", DisplayPhone, key, now); v != verdictNotBonded {
		t.Fatalf("new key: %v, want 05 01 02", v)
	}
	if v := s.Authenticated("A", DisplayPhone, nil, now); v != verdictNotBonded {
		t.Fatalf("stored key before the bond: %v, want 05 01 02", v)
	}
	s.Bonded(DisplayPhone)
	if v := s.Authenticated("A", DisplayPhone, nil, now); v != verdictBonded {
		t.Fatalf("after the bond: %v, want 05 01 01", v)
	}
	if !s.ForgetBond(DisplayPhone) {
		t.Fatal("forget failed")
	}
	if v := s.Authenticated("A", DisplayPhone, nil, now); v != verdictNotBonded {
		t.Fatalf("after forgetting: %v, want 05 01 02", v)
	}
}

// A reconnect with a key that has no bond: 05 01 02, then the certificate phase, the key
// challenge and the bond request again, as G7SensorKit runs them; then glucose.
func TestReconnectWithoutBondBondsAgainThenServes(t *testing.T) {
	s, _ := newTestSensor(t)
	key := []byte("0123456789abcdef")
	s.state.Slots[DisplayWatch] = &Slot{Key: key, LastSeen: time.Now(), Unbonded: true}
	s.Tick(time.Now())
	link := newFakeLink()
	h := newHandler(s, link, AdvertFull)
	defer link.Close()

	clientChallenge := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	h.Write(CharAuth, append(append([]byte{opAuthChallengeTx}, clientChallenge...), DisplayWatch))
	reply := link.recv(t)
	h.Write(CharAuth, append([]byte{opAuthAnswer}, EncryptChallenge(reply.data[9:17], key)...))
	expectNotify(t, link, CharAuth, []byte{opAuthStatus, 0x01, 0x02})

	// The certificate phase, cut to its terminator.
	h.Write(CharAuth, []byte{opCertificate, 0x00, 0, 0, 0, 0})
	expectNotify(t, link, CharAuth, []byte{opCertificate, 0x00, 0x00, 0x00, 0x00})

	// The key challenge: the sensor's challenge, then its 64-byte signature in chunks.
	h.Write(CharAuth, append([]byte{opKeyChallenge}, make([]byte, 16)...))
	if c := link.recv(t); c.char != CharAuth || c.data[0] != opKeyChallenge {
		t.Fatalf("key challenge ack %x", c.data)
	}
	for n := 0; n < 64; {
		c := link.recv(t)
		if c.char != CharCertificate {
			t.Fatalf("signature chunk on %s", c.char)
		}
		n += len(c.data)
	}
	h.Write(CharCertificate, make([]byte, 64))

	h.Write(CharAuth, []byte{opFinalize, 0x1E})
	expectNotify(t, link, CharAuth, []byte{opFinalize, 0x00})
	h.Write(CharAuth, []byte{opBondRequest})
	expectNotify(t, link, CharAuth, []byte{opBondRequest, 0x00})
	expectNotify(t, link, CharAuth, []byte{opBondConfirmed, 0x01})

	h.Subscribed(CharControl)
	h.Write(CharControl, []byte{opGlucose})
	if c := link.recv(t); c.char != CharControl || c.data[0] != opGlucose {
		t.Fatalf("glucose reply %x on %s", c.data, c.char)
	}
	if !bonded(s, DisplayWatch) {
		t.Fatal("not bonded")
	}
}

func TestParseBondMode(t *testing.T) {
	for _, n := range []string{"off", "refuse", "ask"} {
		m, err := ParseBondMode(n)
		if err != nil || m.String() != n {
			t.Fatalf("%s: %v %v", n, m, err)
		}
	}
	if _, err := ParseBondMode("smp"); err == nil {
		t.Fatal("accepted an unknown mode")
	}
}
