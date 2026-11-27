package linux

import (
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/paypal/gatt/linux/cmd"
)

type aclData struct {
	attr  uint16
	flags uint8
	dlen  uint16
	b     []byte
}

func (a *aclData) unmarshal(b []byte) error {
	if len(b) < 4 {
		return fmt.Errorf("malformed acl packet")
	}
	attr := uint16(b[0]) | (uint16(b[1]&0x0f) << 8)
	flags := b[1] >> 4
	dlen := uint16(b[2]) | (uint16(b[3]) << 8)
	if len(b) != 4+int(dlen) {
		return fmt.Errorf("malformed acl packet")
	}

	*a = aclData{attr: attr, flags: flags, dlen: dlen, b: b[4:]}
	return nil
}

type conn struct {
	hci  *HCI
	attr uint16
	aclc chan *aclData

	// wmu keeps the fragments of one PDU together when several goroutines
	// write to the same link.
	wmu sync.Mutex

	// Controller buffers held by this link. The controller never reports
	// completion for packets of a link that has gone away, so these are
	// handed back on disconnection.
	cmu      sync.Mutex
	inflight int
	closed   bool

	// rxCID is the channel of the L2CAP PDU now arriving: only its first
	// fragment names one. Touched by the HCI read loop alone.
	rxCID uint16

	// The Security Manager channel (smp.go).
	smpBuf     []byte
	smpLen     int
	smpc       chan []byte
	smu        sync.Mutex
	smpHandler func(pdu []byte)
}

// The fixed L2CAP channels of an LE link.
const (
	cidATT    = 0x0004
	cidSignal = 0x0005
	cidSMP    = 0x0006
)

func newConn(hci *HCI, hh uint16) *conn {
	c := &conn{
		hci:  hci,
		attr: hh,
		aclc: make(chan *aclData),
		smpc: make(chan []byte, 8),
	}
	go c.smpLoop()
	return c
}

// route hands one ACL packet from the controller to its channel: signaling
// (5), the Security Manager (6), or the attribute protocol (everything else,
// as before). Only the first fragment of an L2CAP PDU carries the header that
// names the channel, so a continuation follows its first fragment. (The
// channel used to be read out of every packet, which took two payload bytes
// of a continuation for it and dropped a continuation shorter than a header.)
func (c *conn) route(a *aclData) {
	if a.flags&0x3 == 0x1 { // a continuing fragment
		switch c.rxCID {
		case 0:
			log.Printf("l2conn: continuation with no PDU open on 0x%04x, dropped", c.attr)
		case cidSignal:
		case cidSMP:
			c.smpFragment(a.b)
		default:
			c.aclc <- a
		}
		return
	}
	if len(a.b) < 4 {
		log.Printf("l2conn: l2cap packet is too short/corrupt, length is %d", len(a.b))
		c.rxCID = 0
		return
	}
	c.rxCID = uint16(a.b[2]) | (uint16(a.b[3]) << 8)
	switch c.rxCID {
	case cidSignal:
		c.handleSignal(a)
	case cidSMP:
		c.smpStart(a.b)
	default:
		c.aclc <- a
	}
}

func (c *conn) updateConnection() (int, error) {
	b := []byte{
		0x12,       // Code (Connection Param Update)
		0x02,       // ID
		0x08, 0x00, // DataLength
		0x08, 0x00, // IntervalMin
		0x18, 0x00, // IntervalMax
		0x00, 0x00, // SlaveLatency
		0xC8, 0x00} // TimeoutMultiplier
	return c.write(0x05, b)
}

// write writes the l2cap payload to the controller.
// It first prepend the l2cap header (4-bytes), and diassemble the payload
// if it is larger than the HCI LE buffer size that the conntroller can support.
func (c *conn) write(cid int, b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	flag := uint8(0) // ACL data continuation flag
	tlen := len(b)   // Total length of the l2cap payload

	// log.Printf("W: [ % X ]", b)
	w := append(
		[]byte{
			0,    // packet type
			0, 0, // attr
			0, 0, // dlen
			uint8(tlen), uint8(tlen >> 8), // l2cap header
			uint8(cid), uint8(cid >> 8), // l2cap header
		}, b...)

	n := 4 + tlen // l2cap header + l2cap payload
	for n > 0 {
		dlen := n
		if dlen > c.hci.bufSize {
			dlen = c.hci.bufSize
		}
		w[0] = 0x02 // packetTypeACL
		w[1] = uint8(c.attr)
		w[2] = uint8(c.attr>>8) | flag
		w[3] = uint8(dlen)
		w[4] = uint8(dlen >> 8)

		// make sure we don't send more buffers than the controller can handdle
		c.hci.bufCnt <- struct{}{}
		c.cmu.Lock()
		if c.closed {
			c.cmu.Unlock()
			<-c.hci.bufCnt
			return 0, io.ErrClosedPipe
		}
		c.inflight++
		c.cmu.Unlock()

		c.hci.d.Write(w[:5+dlen])
		w = w[dlen:] // advance the pointer to the next segment, if any.
		flag = 0x10  // the rest of iterations attr continued segments, if any.
		n -= dlen
	}

	return len(b), nil
}

func (c *conn) Read(b []byte) (int, error) {
	a, ok := <-c.aclc
	if !ok {
		return 0, io.EOF
	}
	tlen := int(uint16(a.b[0]) | uint16(a.b[1])<<8)
	if tlen > len(b) {
		return 0, io.ErrShortBuffer
	}
	d := a.b[4:] // skip l2cap header
	copy(b, d)
	n := len(d)

	// Keep receiving and reassemble continued l2cap segments
	for n != tlen {
		if a, ok = <-c.aclc; !ok || (a.flags&0x1) == 0 {
			return n, io.ErrUnexpectedEOF
		}
		copy(b[n:], a.b)
		n += len(a.b)
	}
	// log.Printf("R: [ % X ]", b[:n])
	return n, nil
}

func (c *conn) Write(b []byte) (int, error) {
	return c.write(0x04, b)
}

// Close disconnects the connection by sending HCI disconnect command to the device.
func (c *conn) Close() error {
	h := c.hci
	hh := c.attr
	h.connsmu.Lock()
	defer h.connsmu.Unlock()
	_, found := h.conns[hh]
	if !found {
		// log.Printf("l2conn: 0x%04x already disconnected", hh)
		return nil
	}
	if err, _ := h.c.Send(cmd.Disconnect{ConnectionHandle: hh, Reason: 0x13}); err != nil {
		return fmt.Errorf("l2conn: failed to disconnect, %s", err)
	}
	return nil
}

// Signal Packets
// 0x00 Reserved								Any
// 0x01 Command reject							0x0001 and 0x0005
// 0x02 Connection request						0x0001
// 0x03 Connection response 					0x0001
// 0x04 Configure request						0x0001
// 0x05 Configure response						0x0001
// 0x06 Disconnection request					0x0001 and 0x0005
// 0x07 Disconnection response					0x0001 and 0x0005
// 0x08 Echo request							0x0001
// 0x09 Echo response							0x0001
// 0x0A Information request						0x0001
// 0x0B Information response					0x0001
// 0x0C Create Channel request					0x0001
// 0x0D Create Channel response					0x0001
// 0x0E Move Channel request					0x0001
// 0x0F Move Channel response					0x0001
// 0x10 Move Channel Confirmation				0x0001
// 0x11 Move Channel Confirmation response		0x0001
// 0x12 Connection Parameter Update request		0x0005
// 0x13 Connection Parameter Update response	0x0005
// 0x14 LE Credit Based Connection request		0x0005
// 0x15 LE Credit Based Connection response		0x0005
// 0x16 LE Flow Control Credit					0x0005
func (c *conn) handleSignal(a *aclData) error {
	log.Printf("ignore l2cap signal:[ % X ]", a.b)
	// FIXME: handle LE signaling channel (CID: 5)
	return nil
}

func (c *conn) completed(n int) {
	c.cmu.Lock()
	if n > c.inflight {
		n = c.inflight
	}
	c.inflight -= n
	c.cmu.Unlock()
	for i := 0; i < n; i++ {
		<-c.hci.bufCnt
	}
}

func (c *conn) released() {
	c.cmu.Lock()
	n := c.inflight
	c.inflight = 0
	c.closed = true
	c.cmu.Unlock()
	for i := 0; i < n; i++ {
		<-c.hci.bufCnt
	}
}
