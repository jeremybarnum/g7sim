package g7

import (
	"fmt"
	"log"
	"sync"
)

// The sensor's part on the Bluetooth security channel, as far as the "request" bond level goes.
//
// A real G7 follows its 07 00 with a Security Request on the link's Security Manager channel
// (L2CAP channel 6), and the display's system decides what happens. From the watch's packet log
// of 11-24 (real sensor DXCMuz, Apple Watch):
//
//	sensor  0B 09                  Security Request: bond, LE Secure Connections
//	watch   05 08                  app not in front: Pairing Failed, unspecified reason, within 1 ms
//
//	sensor  0B 09
//	watch   01 00 00 09 10 03 03   app in front: Pairing Request, 3 ms later
//	sensor  02 03 00 09 10 02 03   Pairing Response, 0.15 s later; on it the watch shows its Pair prompt
//	watch   0C <its public key>    only when the person taps Pair (3 ms after the tap)
//	...                            public key, confirm, random and key check each way, then encryption
//
// Neither side asks for protection against a man in the middle and the sensor has no display or
// keys, so the pairing is "just works": the prompt offers Pair and Cancel and nothing to compare.
//
// This level sends the Security Request and the Pairing Response, which are fixed bytes, and
// stops there: it makes no key and encrypts nothing. The display's public key says the person
// tapped Pair; the rest of the pairing is declined with Pairing Failed, "pairing not supported".
// Real pairing in the emulator's stack is the level after this one.

// Security Manager opcodes.
const (
	smpPairingRequest   = 0x01
	smpPairingResponse  = 0x02
	smpPairingFailed    = 0x05
	smpSecurityRequest  = 0x0B
	smpPairingPublicKey = 0x0C
)

// Bits of the authentication requirements byte.
const (
	smpAuthBond = 0x01
	smpAuthMITM = 0x04
	smpAuthSC   = 0x08 // LE Secure Connections
)

// Keys a side offers to hand over once the link is encrypted.
const (
	smpKeyEncryption = 0x01
	smpKeyIdentity   = 0x02
)

const (
	smpNoInputNoOutput = 0x03 // the sensor: no display, no keys

	smpReasonNotSupported = 0x05 // pairing not supported
	smpReasonUnspecified  = 0x08 // what an Apple Watch or iPhone answers with its app not in front
)

// sensorAuthReq is what a real G7 asks for, in its Security Request and its Pairing Response.
const sensorAuthReq = smpAuthBond | smpAuthSC

// pairingFeatures is the body of a Pairing Request or a Pairing Response.
type pairingFeatures struct {
	IOCapability  byte
	OOB           byte
	AuthReq       byte
	MaxKeySize    byte
	InitiatorKeys byte
	ResponderKeys byte
}

func parsePairingFeatures(pdu []byte) (pairingFeatures, bool) {
	if len(pdu) != 7 || (pdu[0] != smpPairingRequest && pdu[0] != smpPairingResponse) {
		return pairingFeatures{}, false
	}
	return pairingFeatures{pdu[1], pdu[2], pdu[3], pdu[4], pdu[5], pdu[6]}, true
}

func (f pairingFeatures) pdu(opcode byte) []byte {
	return []byte{opcode, f.IOCapability, f.OOB, f.AuthReq, f.MaxKeySize, f.InitiatorKeys, f.ResponderKeys}
}

func securityRequest() []byte { return []byte{smpSecurityRequest, sensorAuthReq} }

func pairingFailed(reason byte) []byte { return []byte{smpPairingFailed, reason} }

// sensorPairingResponse is a real G7's answer to a Pairing Request: 02 03 00 09 10 02 03 to the
// watch's 01 00 00 09 10 03 03. It asks the display for its identity key and offers its own
// encryption and identity keys. A responder may name only keys the request named, so the two key
// fields are cut down to the request's; against the watch's request that changes nothing.
func sensorPairingResponse(request pairingFeatures) pairingFeatures {
	return pairingFeatures{
		IOCapability:  smpNoInputNoOutput,
		AuthReq:       sensorAuthReq,
		MaxKeySize:    16,
		InitiatorKeys: request.InitiatorKeys & smpKeyIdentity,
		ResponderKeys: request.ResponderKeys & (smpKeyEncryption | smpKeyIdentity),
	}
}

var smpReasons = map[byte]string{
	0x01: "passkey entry failed", 0x02: "no out-of-band data", 0x03: "authentication requirements",
	0x04: "confirm value failed", 0x05: "pairing not supported", 0x06: "encryption key size",
	0x07: "command not supported", 0x08: "unspecified reason", 0x09: "repeated attempts",
	0x0A: "invalid parameters", 0x0B: "key check failed", 0x0C: "numeric comparison failed",
}

func smpReason(reason byte) string {
	if s, ok := smpReasons[reason]; ok {
		return s
	}
	return "reason unknown"
}

func describeAuthReq(a byte) string {
	s := "no bond"
	if a&smpAuthBond != 0 {
		s = "bond"
	}
	if a&smpAuthSC != 0 {
		s += ", LE Secure Connections"
	} else {
		s += ", legacy pairing"
	}
	if a&smpAuthMITM != 0 {
		s += ", protection against a man in the middle"
	}
	return s
}

// describeSMP puts a Security Manager PDU in a log line. Only the messages that carry settings
// are shown in full: the others carry key material, which stays out of the log.
func describeSMP(pdu []byte) string {
	if len(pdu) == 0 {
		return "empty message"
	}
	switch {
	case pdu[0] == smpSecurityRequest && len(pdu) == 2:
		return fmt.Sprintf("Security Request % x (%s)", pdu, describeAuthReq(pdu[1]))
	case pdu[0] == smpPairingFailed && len(pdu) == 2:
		return fmt.Sprintf("Pairing Failed % x (%s)", pdu, smpReason(pdu[1]))
	case pdu[0] == smpPairingPublicKey:
		return fmt.Sprintf("Pairing Public Key (%d bytes, not shown)", len(pdu))
	}
	if f, ok := parsePairingFeatures(pdu); ok {
		name := "Pairing Request"
		if pdu[0] == smpPairingResponse {
			name = "Pairing Response"
		}
		return fmt.Sprintf("%s % x (input and output %d, %s, key size %d, keys asked %02x, keys offered %02x)",
			name, pdu, f.IOCapability, describeAuthReq(f.AuthReq), f.MaxKeySize, f.InitiatorKeys, f.ResponderKeys)
	}
	return fmt.Sprintf("message %02x (%d bytes, not shown)", pdu[0], len(pdu))
}

// What the display's system did with the Security Request.
type pairingStep int

const (
	pairingPrompted pairingStep = iota // it sent a Pairing Request and has the sensor's response: its Pair prompt shows
	pairingAccepted                    // it sent its public key: the person tapped Pair
	pairingRefused                     // it sent Pairing Failed
)

type pairingEvent struct {
	step   pairingStep
	reason byte // of a refusal
}

// pairing is one link's exchange on the security channel. The transport calls received from a
// goroutine of its own; the handler follows the exchange through events.
type pairing struct {
	link   SecurityLink
	addr   string
	events chan pairingEvent

	mu       sync.Mutex
	prompted bool
	over     bool
}

// requestPairing sends the sensor's Security Request and answers what comes back.
func requestPairing(link SecurityLink, addr string) (*pairing, error) {
	p := &pairing{link: link, addr: addr, events: make(chan pairingEvent, 8)}
	link.OnSMP(p.received)
	if err := p.send(securityRequest()); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *pairing) send(pdu []byte) error {
	log.Printf("%s security -> %s", p.addr, describeSMP(pdu))
	return p.link.SendSMP(pdu)
}

func (p *pairing) emit(e pairingEvent) {
	select {
	case p.events <- e:
	default:
	}
}

func (p *pairing) received(pdu []byte) {
	if len(pdu) == 0 {
		return
	}
	log.Printf("%s security <- %s", p.addr, describeSMP(pdu))
	p.mu.Lock()
	defer p.mu.Unlock()

	request, isRequest := parsePairingFeatures(pdu)
	isRequest = isRequest && pdu[0] == smpPairingRequest
	switch {
	case pdu[0] == smpPairingFailed:
		// Never answered. Before the pairing is over it is the display's system refusing, or the
		// person tapping Cancel.
		if !p.over {
			p.over = true
			reason := byte(0)
			if len(pdu) > 1 {
				reason = pdu[1]
			}
			p.emit(pairingEvent{step: pairingRefused, reason: reason})
		}
	case isRequest && !p.over:
		p.send(sensorPairingResponse(request).pdu(smpPairingResponse))
		p.prompted = true
		p.emit(pairingEvent{step: pairingPrompted})
	case pdu[0] == smpPairingPublicKey && p.prompted && !p.over:
		p.over = true
		p.send(pairingFailed(smpReasonNotSupported))
		p.emit(pairingEvent{step: pairingAccepted})
	default:
		// Out of turn, or after the end: there is no pairing here to carry on.
		p.send(pairingFailed(smpReasonNotSupported))
	}
}
