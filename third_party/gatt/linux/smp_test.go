package linux

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// fakeDevice records what the stack writes to the controller.
type fakeDevice struct {
	mu     sync.Mutex
	writes [][]byte
}

func (d *fakeDevice) Read([]byte) (int, error) { select {} }
func (d *fakeDevice) Close() error             { return nil }
func (d *fakeDevice) Write(b []byte) (int, error) {
	d.mu.Lock()
	d.writes = append(d.writes, append([]byte(nil), b...))
	d.mu.Unlock()
	return len(b), nil
}

func (d *fakeDevice) written() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.writes...)
}

const testHandle = 0x0040

// testLink is a link on a controller that is not there: packets are fed in as the controller
// would deliver them, and the attribute channel's reader and the Security Manager handler
// record what reaches them.
type testLink struct {
	t   *testing.T
	hci *HCI
	dev *fakeDevice
	c   *conn
	att chan []byte
	smp chan []byte
}

func newTestLink(t *testing.T, handleSMP bool) *testLink {
	dev := &fakeDevice{}
	h := &HCI{
		d:       dev,
		bufCnt:  make(chan struct{}, 15-1),
		bufSize: 27,
		connsmu: &sync.Mutex{},
		conns:   map[uint16]*conn{},
	}
	l := &testLink{t: t, hci: h, dev: dev, c: newConn(h, testHandle), att: make(chan []byte, 8), smp: make(chan []byte, 8)}
	h.conns[testHandle] = l.c
	if handleSMP {
		l.c.SetSMPHandler(func(pdu []byte) { l.smp <- append([]byte(nil), pdu...) })
	}
	go func() {
		for {
			b := make([]byte, 672)
			n, err := l.c.Read(b)
			if err != nil {
				return
			}
			l.att <- b[:n]
		}
	}()
	t.Cleanup(func() {
		close(l.c.aclc)
		close(l.c.smpc)
	})
	return l
}

// first builds the HCI packet of the first fragment of an L2CAP PDU: total is the PDU's whole
// payload length, part the bytes of it in this fragment.
func first(cid uint16, total int, part []byte) []byte {
	l2cap := append([]byte{byte(total), byte(total >> 8), byte(cid), byte(cid >> 8)}, part...)
	return acl(0x2, l2cap)
}

func continuation(part []byte) []byte { return acl(0x1, part) }

func acl(pb byte, data []byte) []byte {
	return append([]byte{0x02, byte(testHandle & 0xff), byte(testHandle>>8) | pb<<4, byte(len(data)), byte(len(data) >> 8)}, data...)
}

func (l *testLink) feed(packets ...[]byte) {
	for _, p := range packets {
		l.hci.handlePacket(p)
	}
}

func expect(t *testing.T, name string, ch chan []byte, want []byte) {
	t.Helper()
	select {
	case got := <-ch:
		if !bytes.Equal(got, want) {
			t.Fatalf("%s got % x, want % x", name, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s got nothing, want % x", name, want)
	}
}

func expectNone(t *testing.T, name string, ch chan []byte) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s got % x, want nothing", name, got)
	case <-time.After(50 * time.Millisecond):
	}
}

// Channel 6 goes to the Security Manager handler and no longer to the attribute protocol.
func TestSecurityManagerChannelReachesItsHandler(t *testing.T) {
	l := newTestLink(t, true)
	request := []byte{0x01, 0x00, 0x00, 0x09, 0x10, 0x03, 0x03}
	l.feed(first(cidSMP, len(request), request))
	expect(t, "the security manager", l.smp, request)
	expectNone(t, "the attribute protocol", l.att)
}

// Channel 4 still goes to the attribute protocol, and channel 5 to neither.
func TestOtherChannelsGoWhereTheyDid(t *testing.T) {
	l := newTestLink(t, true)
	write := []byte{0x52, 0x1d, 0x00, 0x07}
	l.feed(first(cidATT, len(write), write))
	expect(t, "the attribute protocol", l.att, write)

	signal := []byte{0x12, 0x02, 0x08, 0x00, 0x08, 0x00, 0x18, 0x00, 0x00, 0x00, 0xc8, 0x00}
	l.feed(first(cidSignal, len(signal), signal))
	expectNone(t, "the attribute protocol", l.att)
	expectNone(t, "the security manager", l.smp)
}

// A pairing public key is 65 bytes and arrives in fragments (27, 17 and 25 bytes of data from a
// real sensor on 11-24). The handler gets it once, whole.
func TestSecurityManagerPDUIsReassembled(t *testing.T) {
	l := newTestLink(t, true)
	key := append([]byte{0x0C}, bytes.Repeat([]byte{0xEE}, 64)...)
	// The second fragment's bytes 2 and 3 read as "channel 4" and the third's as "channel 5":
	// a continuation has no header, and must follow its first fragment.
	key[23+2], key[23+3] = 0x04, 0x00
	key[40+2], key[40+3] = 0x05, 0x00
	l.feed(first(cidSMP, len(key), key[:23]))
	expectNone(t, "the security manager", l.smp)
	l.feed(continuation(key[23:40]), continuation(key[40:]))
	expect(t, "the security manager", l.smp, key)
	expectNone(t, "the attribute protocol", l.att)

	// The next PDU starts clean.
	failed := []byte{0x05, 0x08}
	l.feed(first(cidSMP, len(failed), failed))
	expect(t, "the security manager", l.smp, failed)
}

// A fragmented attribute write whose continuation reads as "channel 6", or is shorter than a
// header, still reaches the attribute protocol whole.
func TestAttributeContinuationIsNotTakenForAnotherChannel(t *testing.T) {
	l := newTestLink(t, true)
	write := append([]byte{0x12, 0x1d, 0x00}, bytes.Repeat([]byte{0xAA}, 27)...)
	write[23+2], write[23+3] = 0x06, 0x00
	l.feed(first(cidATT, len(write), write[:23]), continuation(write[23:27]), continuation(write[27:]))
	expect(t, "the attribute protocol", l.att, write)
	expectNone(t, "the security manager", l.smp)
}

// With no handler the stack declines: Pairing Failed, pairing not supported, on channel 6.
func TestPairingIsDeclinedWithNoHandler(t *testing.T) {
	l := newTestLink(t, false)
	request := []byte{0x01, 0x00, 0x00, 0x09, 0x10, 0x03, 0x03}
	l.feed(first(cidSMP, len(request), request))
	want := []byte{0x02, testHandle & 0xff, testHandle >> 8, 0x06, 0x00, 0x02, 0x00, 0x06, 0x00, 0x05, 0x05}
	deadline := time.Now().Add(2 * time.Second)
	for len(l.dev.written()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := l.dev.written(); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("wrote % x, want % x", got, want)
	}
	expectNone(t, "the attribute protocol", l.att)

	// A Pairing Failed from the peer is not answered.
	l.feed(first(cidSMP, 2, []byte{0x05, 0x08}))
	time.Sleep(50 * time.Millisecond)
	if got := l.dev.written(); len(got) != 1 {
		t.Fatalf("answered a Pairing Failed: % x", got[1:])
	}
}

// A Security Request goes out on channel 6 with its L2CAP header.
func TestWriteSMPUsesTheSecurityManagerChannel(t *testing.T) {
	l := newTestLink(t, true)
	if _, err := l.c.WriteSMP([]byte{0x0B, 0x09}); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x02, testHandle & 0xff, testHandle >> 8, 0x06, 0x00, 0x02, 0x00, 0x06, 0x00, 0x0B, 0x09}
	if got := l.dev.written(); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("wrote % x, want % x", got, want)
	}
}

// A PDU longer than any Security Manager message is dropped, fragments and all.
func TestOversizedSecurityManagerPDUIsDropped(t *testing.T) {
	l := newTestLink(t, true)
	l.feed(first(cidSMP, 200, bytes.Repeat([]byte{0x01}, 23)), continuation(bytes.Repeat([]byte{0x01}, 27)))
	expectNone(t, "the security manager", l.smp)
	failed := []byte{0x05, 0x08}
	l.feed(first(cidSMP, len(failed), failed))
	expect(t, "the security manager", l.smp, failed)
}
