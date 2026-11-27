package g7

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// The messages recorded on 11-24 between a real G7 and an Apple Watch (the watch's packet log).
var (
	recordedSecurityRequest = []byte{0x0B, 0x09}
	recordedPairingRequest  = []byte{0x01, 0x00, 0x00, 0x09, 0x10, 0x03, 0x03}
	recordedPairingResponse = []byte{0x02, 0x03, 0x00, 0x09, 0x10, 0x02, 0x03}
	recordedRefusal         = []byte{0x05, 0x08}
)

// A stand-in public key message: the opcode and 64 bytes that are no key.
func publicKeyMessage() []byte {
	return append([]byte{smpPairingPublicKey}, bytes.Repeat([]byte{0xEE}, 64)...)
}

func TestSecurityRequestIsTheSensors(t *testing.T) {
	if got := securityRequest(); !bytes.Equal(got, recordedSecurityRequest) {
		t.Fatalf("security request % x, want % x", got, recordedSecurityRequest)
	}
}

func TestPairingResponseToTheWatchIsTheSensors(t *testing.T) {
	request, ok := parsePairingFeatures(recordedPairingRequest)
	if !ok {
		t.Fatal("the watch's Pairing Request did not parse")
	}
	want := pairingFeatures{IOCapability: 0x00, AuthReq: 0x09, MaxKeySize: 16, InitiatorKeys: 0x03, ResponderKeys: 0x03}
	if request != want {
		t.Fatalf("parsed %+v, want %+v", request, want)
	}
	if got := sensorPairingResponse(request).pdu(smpPairingResponse); !bytes.Equal(got, recordedPairingResponse) {
		t.Fatalf("pairing response % x, want % x", got, recordedPairingResponse)
	}
}

// A responder may name only the keys the request named.
func TestPairingResponseNamesNoKeyTheRequestLeftOut(t *testing.T) {
	got := sensorPairingResponse(pairingFeatures{AuthReq: 0x09, MaxKeySize: 16, InitiatorKeys: 0x01, ResponderKeys: 0x01})
	if got.InitiatorKeys != 0x00 || got.ResponderKeys != 0x01 {
		t.Fatalf("keys %02x/%02x, want 00/01", got.InitiatorKeys, got.ResponderKeys)
	}
}

func TestPairingFeaturesRejectOtherMessages(t *testing.T) {
	for _, pdu := range [][]byte{nil, {0x01}, recordedPairingRequest[:6], append([]byte{0x03}, make([]byte, 6)...), recordedRefusal} {
		if _, ok := parsePairingFeatures(pdu); ok {
			t.Fatalf("% x parsed as pairing features", pdu)
		}
	}
}

func TestPairingFailedMessage(t *testing.T) {
	if got := pairingFailed(smpReasonNotSupported); !bytes.Equal(got, []byte{0x05, 0x05}) {
		t.Fatalf("pairing failed % x", got)
	}
}

// Log lines name the settings and never carry a key.
func TestDescribeSMPKeepsKeysOutOfTheLog(t *testing.T) {
	for pdu, want := range map[string]string{
		string(recordedSecurityRequest): "Security Request 0b 09 (bond, LE Secure Connections)",
		string(recordedRefusal):         "Pairing Failed 05 08 (unspecified reason)",
		string(recordedPairingRequest):  "Pairing Request 01 00 00 09 10 03 03 (input and output 0, bond, LE Secure Connections, key size 16, keys asked 03, keys offered 03)",
	} {
		if got := describeSMP([]byte(pdu)); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	for _, pdu := range [][]byte{publicKeyMessage(), append([]byte{0x03}, bytes.Repeat([]byte{0xEE}, 16)...), append([]byte{0x06}, bytes.Repeat([]byte{0xEE}, 16)...)} {
		if got := describeSMP(pdu); strings.Contains(strings.ToLower(got), "ee") || !strings.Contains(got, "not shown") {
			t.Fatalf("% x logged as %q", pdu[:1], got)
		}
	}
}

// fakeSecurityLink is a link with a security channel: it records what the sensor sends there and
// lets a test play the display's system.
type fakeSecurityLink struct {
	*fakeLink
	sent chan []byte

	mu      sync.Mutex
	handler func([]byte)
}

func newFakeSecurityLink() *fakeSecurityLink {
	return &fakeSecurityLink{fakeLink: newFakeLink(), sent: make(chan []byte, 16)}
}

func (l *fakeSecurityLink) SendSMP(pdu []byte) error {
	l.sent <- append([]byte(nil), pdu...)
	return nil
}

func (l *fakeSecurityLink) OnSMP(f func([]byte)) {
	l.mu.Lock()
	l.handler = f
	l.mu.Unlock()
}

// system plays a message from the display's system.
func (l *fakeSecurityLink) system(t *testing.T, pdu []byte) {
	t.Helper()
	l.mu.Lock()
	f := l.handler
	l.mu.Unlock()
	if f == nil {
		t.Fatal("the sensor is not listening on the security channel")
	}
	f(pdu)
}

func (l *fakeSecurityLink) expectSent(t *testing.T, want []byte) {
	t.Helper()
	select {
	case got := <-l.sent:
		if !bytes.Equal(got, want) {
			t.Fatalf("sensor sent % x on the security channel, want % x", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the sensor did not send % x on the security channel", want)
	}
}

func (l *fakeSecurityLink) expectNothingSent(t *testing.T) {
	t.Helper()
	select {
	case got := <-l.sent:
		t.Fatalf("sensor sent % x on the security channel", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// requestWith runs the finalize step at the request level, with a new, unbonded watch key, up to
// the sensor's Security Request.
func requestWith(t *testing.T, keepAlive byte) (*Sensor, *fakeSecurityLink, chan bool) {
	t.Helper()
	s, _ := newTestSensor(t)
	s.SetBondMode(BondRequest)
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: time.Now(), Unbonded: true}
	link := newFakeSecurityLink()
	h := bareHandler(s, link, DisplayWatch)
	result := make(chan bool, 1)
	go func() { result <- h.finalize() }()
	h.in <- inMsg{char: CharAuth, data: []byte{opFinalize, keepAlive}}
	expectNotify(t, link.fakeLink, CharAuth, []byte{opFinalize, 0x00})
	h.in <- inMsg{char: CharAuth, data: []byte{opBondRequest}}
	expectNotify(t, link.fakeLink, CharAuth, []byte{opBondRequest, 0x00})
	link.expectSent(t, recordedSecurityRequest)
	return s, link, result
}

func expectHangUp(t *testing.T, result chan bool, within time.Duration) {
	t.Helper()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("reported as bonded")
		}
	case <-time.After(within):
		t.Fatal("the sensor did not hang up")
	}
}

func pendingBonds(s *Sensor) []map[string]interface{} {
	return s.Status()["bondPending"].([]map[string]interface{})
}

// The app is not in front: the system refuses at once, and the sensor hangs up and serves nothing.
func TestBondRequestRefusedByTheSystem(t *testing.T) {
	s, link, result := requestWith(t, 0x1E)
	link.system(t, recordedRefusal)
	expectHangUp(t, result, 2*time.Second)
	link.expectNothingSent(t) // a Pairing Failed is never answered
	expectSilence(t, link.fakeLink, 50*time.Millisecond)
	if bonded(s, DisplayWatch) {
		t.Fatal("slot marked bonded")
	}
	if s.SlotKey(DisplayWatch) == nil {
		t.Fatal("the key was dropped; a real G7 keeps it")
	}
}

// The app is in front: the system sends its Pairing Request, gets the sensor's response and shows
// its prompt. The person taps Pair: its public key arrives, the rest is declined, and the sensor
// goes on as bonded.
func TestBondRequestPairTappedOnTheDisplay(t *testing.T) {
	s, link, result := requestWith(t, 0x1E)
	link.system(t, recordedPairingRequest)
	link.expectSent(t, recordedPairingResponse)
	expectSilence(t, link.fakeLink, 100*time.Millisecond) // no 08 01 while the prompt shows
	if got := pendingBonds(s); len(got) != 1 || got[0]["displayType"] != int(DisplayWatch) || got[0]["onDevice"] != true {
		t.Fatalf("pending %v, want the watch's own prompt", got)
	}

	link.system(t, publicKeyMessage())
	link.expectSent(t, []byte{0x05, 0x05})
	expectNotify(t, link.fakeLink, CharAuth, []byte{opBondConfirmed, 0x01})
	if !<-result || !bonded(s, DisplayWatch) {
		t.Fatal("not bonded after Pair on the display")
	}
	if got := pendingBonds(s); len(got) != 0 {
		t.Fatalf("prompt still pending: %v", got)
	}
}

// Pair from the page stands in while the display's prompt shows. A Pair on the display after
// that is still answered, so the display's system is not left waiting.
func TestBondRequestPairGivenOnThePage(t *testing.T) {
	s, link, result := requestWith(t, 0x1E)
	if got := s.AcceptBond(0); len(got) != 0 {
		t.Fatalf("Pair accepted before any prompt showed: %v", got)
	}
	link.system(t, recordedPairingRequest)
	link.expectSent(t, recordedPairingResponse)
	for len(pendingBonds(s)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.AcceptBond(DisplayWatch); len(got) != 1 {
		t.Fatalf("accepted %v", got)
	}
	expectNotify(t, link.fakeLink, CharAuth, []byte{opBondConfirmed, 0x01})
	if !<-result || !bonded(s, DisplayWatch) {
		t.Fatal("not bonded after Pair on the page")
	}
	link.system(t, publicKeyMessage())
	link.expectSent(t, []byte{0x05, 0x05})
}

// Cancel on the display's prompt: the system sends Pairing Failed and the sensor hangs up.
func TestBondRequestCancelledOnTheDisplay(t *testing.T) {
	s, link, result := requestWith(t, 0x1E)
	link.system(t, recordedPairingRequest)
	link.expectSent(t, recordedPairingResponse)
	link.system(t, recordedRefusal)
	expectHangUp(t, result, 2*time.Second)
	expectSilence(t, link.fakeLink, 50*time.Millisecond)
	if bonded(s, DisplayWatch) || len(pendingBonds(s)) != 0 {
		t.Fatal("bonded, or the prompt outlived the link")
	}
}

// Nobody answers the display's prompt: the sensor hangs up when the keep-alive runs out.
func TestBondRequestPromptUnanswered(t *testing.T) {
	shortGrace(t)
	s, link, result := requestWith(t, 0x01) // a 1 s keep-alive
	link.system(t, recordedPairingRequest)
	link.expectSent(t, recordedPairingResponse)
	expectHangUp(t, result, 3*time.Second)
	link.expectNothingSent(t)
	expectSilence(t, link.fakeLink, 50*time.Millisecond)
	if bonded(s, DisplayWatch) || len(s.AcceptBond(0)) != 0 {
		t.Fatal("bonded, or the prompt outlived the link")
	}
}

// The system never answers the Security Request: the sensor hangs up when the keep-alive runs out.
func TestBondRequestNeverAnswered(t *testing.T) {
	shortGrace(t)
	s, _, result := requestWith(t, 0x01)
	expectHangUp(t, result, 3*time.Second)
	if bonded(s, DisplayWatch) {
		t.Fatal("slot marked bonded")
	}
}

// A public key with no Pairing Request before it is not a Pair: it is declined and the sensor
// keeps waiting.
func TestBondRequestPublicKeyOutOfTurnIsNotPair(t *testing.T) {
	s, link, result := requestWith(t, 0x1E)
	link.system(t, publicKeyMessage())
	link.expectSent(t, []byte{0x05, 0x05})
	expectSilence(t, link.fakeLink, 100*time.Millisecond)
	select {
	case <-result:
		t.Fatal("the bond step ended on an out-of-turn message")
	default:
	}
	if bonded(s, DisplayWatch) {
		t.Fatal("slot marked bonded")
	}
}

// The Simulator's link has no security channel: the request level asks for Pair on the page.
func TestBondRequestWithoutASecurityChannelAsks(t *testing.T) {
	s, _ := newTestSensor(t)
	s.SetBondMode(BondRequest)
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: time.Now(), Unbonded: true}
	link := newFakeLink()
	h := bareHandler(s, link, DisplayWatch)
	result := make(chan bool, 1)
	go func() { result <- h.finalize() }()
	h.in <- inMsg{char: CharAuth, data: []byte{opFinalize, 0x1E}}
	expectNotify(t, link, CharAuth, []byte{opFinalize, 0x00})
	h.in <- inMsg{char: CharAuth, data: []byte{opBondRequest}}
	expectNotify(t, link, CharAuth, []byte{opBondRequest, 0x00})
	for len(pendingBonds(s)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := pendingBonds(s); got[0]["onDevice"] != false {
		t.Fatalf("pending %v, want the page's prompt", got)
	}
	s.AcceptBond(DisplayWatch)
	expectNotify(t, link, CharAuth, []byte{opBondConfirmed, 0x01})
	if !<-result || !bonded(s, DisplayWatch) {
		t.Fatal("not bonded")
	}
}
