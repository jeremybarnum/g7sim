package linux

import "log"

// The Security Manager channel (L2CAP channel 6) of a link.
//
// This stack has no security manager: it generates no keys and never encrypts
// a link. What is here is the channel only. Whole Security Manager PDUs from
// the peer go to the handler the package's user sets, and the user may write
// PDUs of its own (a peripheral's Security Request, for one). With no handler
// set, every pairing PDU is declined with Pairing Failed, "pairing not
// supported", which is what the specification asks of a device that cannot
// pair. (These PDUs used to be read as attribute requests and answered with an
// attribute error on channel 4.)

const (
	smpMaxPDU = 65 // the largest Security Manager PDU: a pairing public key

	smpOpPairingFailed          = 0x05
	smpReasonPairingUnsupported = 0x05
)

// WriteSMP sends one Security Manager PDU to the peer.
func (c *conn) WriteSMP(pdu []byte) (int, error) {
	return c.write(cidSMP, pdu)
}

// SetSMPHandler has f called with each whole Security Manager PDU the peer
// sends, in order, on a goroutine of the link's own. nil restores the default,
// which declines.
func (c *conn) SetSMPHandler(f func(pdu []byte)) {
	c.smu.Lock()
	c.smpHandler = f
	c.smu.Unlock()
}

// smpStart takes the first fragment of a PDU on the channel, L2CAP header
// included.
func (c *conn) smpStart(b []byte) {
	c.smpBuf = nil
	c.smpLen = int(uint16(b[0]) | uint16(b[1])<<8)
	if c.smpLen == 0 || c.smpLen > smpMaxPDU {
		log.Printf("l2conn: security manager PDU of %d bytes on 0x%04x, dropped", c.smpLen, c.attr)
		c.smpLen = 0
		return
	}
	c.smpBuf = make([]byte, 0, c.smpLen)
	c.smpFragment(b[4:])
}

// smpFragment adds a fragment's payload and passes the PDU on once it is whole.
func (c *conn) smpFragment(b []byte) {
	if c.smpLen == 0 {
		return // the rest of a PDU that was dropped
	}
	c.smpBuf = append(c.smpBuf, b...)
	if len(c.smpBuf) < c.smpLen {
		return
	}
	pdu := c.smpBuf[:c.smpLen]
	c.smpBuf, c.smpLen = nil, 0
	// Never block the HCI read loop on the handler.
	select {
	case c.smpc <- pdu:
	default:
		log.Printf("l2conn: security manager queue full on 0x%04x, dropped opcode 0x%02x", c.attr, pdu[0])
	}
}

// smpLoop runs the handler for the life of the link.
func (c *conn) smpLoop() {
	for pdu := range c.smpc {
		c.smu.Lock()
		f := c.smpHandler
		c.smu.Unlock()
		if f != nil {
			f(pdu)
			continue
		}
		// Opcode and length only: some of these PDUs carry key material.
		if pdu[0] == smpOpPairingFailed {
			log.Printf("l2conn: security manager opcode 0x05 (%d bytes) from 0x%04x, ignored", len(pdu), c.attr)
			continue
		}
		log.Printf("l2conn: security manager opcode 0x%02x (%d bytes) from 0x%04x, declined: pairing not supported", pdu[0], len(pdu), c.attr)
		c.WriteSMP([]byte{smpOpPairingFailed, smpReasonPairingUnsupported})
	}
}
