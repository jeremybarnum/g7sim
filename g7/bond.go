package g7

import (
	"fmt"
	"log"
	"sort"
	"time"
)

// The Bluetooth bond at the end of a pairing.
//
// A real G7 answers a new key with 05 01 02 (authenticated, no bond). The display writes 06 1E
// (answered 06 00) and the bond request 07; the sensor answers 07 00 and, in the same
// millisecond, sends a Bluetooth security request. What happens next is up to the display's
// system:
//
//   - it pairs: the link is encrypted, the sensor sends 08 01 about 0.1 s later and serves
//     glucose, and at later connections the verdict is 05 01 01;
//   - it refuses (an iPhone or Apple Watch whose app is not in front): the sensor hangs up
//     about 0.2 s later;
//   - it shows its Pair prompt and nobody answers: the sensor hangs up about 31 s after the
//     request.
//
// An unbonded display gets no glucose. Its key is kept and authenticates at later connections
// with 05 01 02, after which the display runs the certificate phase and asks to bond again.
// (Seen 11-24, real sensor and Apple Watch, in the watch's Bluetooth packet logs. When a real
// sensor discards a key that never bonded is not settled; here it never does.)
//
// This emulator cannot pair, so three levels play the display's system's part: BondInstant bonds
// at once (unattended test runs), BondRefuse is the system refusing, and BondAsk holds the link
// for an operator's Pair, as a person must tap it on the device. BondRequest leaves the part to
// the real system: it sends the sensor's Security Request (smp.go), over a Bluetooth adapter
// only. A client can also hold its own bond request until its app is in front (see finalize).

type BondMode int

const (
	BondInstant BondMode = iota // the display's system pairs at once
	BondRefuse                  // the display's system refuses: hang up, no glucose
	BondAsk                     // wait for an operator's Pair, then bond; hang up if none comes
	BondRequest                 // send a real Security Request: the display's own system refuses, or shows its Pair prompt
)

var bondModeNames = [...]string{"off", "refuse", "ask", "request"}

func (m BondMode) String() string {
	if int(m) < len(bondModeNames) {
		return bondModeNames[m]
	}
	return fmt.Sprint(int(m))
}

// ParseBondMode reads the -bond flag and the API's mode: off, refuse, ask or request.
func ParseBondMode(s string) (BondMode, error) {
	for i, n := range bondModeNames {
		if s == n {
			return BondMode(i), nil
		}
	}
	return 0, fmt.Errorf("bond mode %q: want off, refuse, ask or request", s)
}

// The sensor hangs up 0.2 s after a refused security request. An unanswered Pair prompt lasts as
// long as the display's keep-alive holds the link (about 31 s after 06 1E).
var bondRefuseHangUp = 200 * time.Millisecond

type bondRequest struct {
	addr     string
	deadline time.Time
	onDevice bool // the display's own system is showing the prompt; Pair here stands in for it
	accepted chan struct{}
}

func (s *Sensor) SetBondMode(m BondMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bondMode = m
}

func (s *Sensor) BondMode() BondMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bondMode
}

// Bonded records that a display's system paired with the sensor.
func (s *Sensor) Bonded(displayType byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.state.Slots[displayType]; slot != nil && slot.Unbonded {
		slot.Unbonded = false
		s.save()
	}
}

// ForgetBond is the display forgetting the sensor in its Bluetooth settings: its key still
// authenticates, but it must bond again before it gets glucose.
func (s *Sensor) ForgetBond(displayType byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.state.Slots[displayType]
	if slot == nil {
		return false
	}
	slot.Unbonded = true
	s.save()
	return true
}

// requestBond shows the display's Pair prompt until the deadline. The channel closes when an
// operator accepts it. onDevice says the display's own system is showing a prompt too.
func (s *Sensor) requestBond(addr string, displayType byte, deadline time.Time, onDevice bool) *bondRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &bondRequest{addr: addr, deadline: deadline, onDevice: onDevice, accepted: make(chan struct{})}
	s.bondPending[displayType] = r
	return r
}

func (s *Sensor) endBondRequest(displayType byte, r *bondRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bondPending[displayType] == r {
		delete(s.bondPending, displayType)
	}
}

// AcceptBond is the person tapping Pair on the display's prompt. Display type 0 accepts every
// prompt showing. Returns the display types accepted.
func (s *Sensor) AcceptBond(displayType byte) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for t, r := range s.bondPending {
		if displayType != 0 && t != displayType {
			continue
		}
		close(r.accepted)
		delete(s.bondPending, t)
		out = append(out, int(t))
	}
	sort.Ints(out)
	return out
}

// pendingBonds lists the prompts showing, for the status. Caller holds s.mu.
func (s *Sensor) pendingBonds(now time.Time) []map[string]interface{} {
	out := []map[string]interface{}{}
	var types []int
	for t := range s.bondPending {
		types = append(types, int(t))
	}
	sort.Ints(types)
	for _, t := range types {
		r := s.bondPending[byte(t)]
		left := r.deadline.Sub(now).Seconds()
		if left < 0 {
			left = 0
		}
		out = append(out, map[string]interface{}{"displayType": t, "addr": r.addr, "secondsLeft": int(left), "onDevice": r.onDevice})
	}
	return out
}

// bond settles the Bluetooth bond after the sensor's 07 00: it plays the display's system, or at
// the request level asks the real one. It returns whether the link is now bonded.
func (h *handler) bond() bool {
	switch h.sensor.BondMode() {
	case BondRefuse:
		log.Printf("%s: display type %d's system refused the Bluetooth pairing; hanging up", h.link.Addr(), h.displayType)
		h.refusedHangUp()
		return false

	case BondAsk:
		if !h.askForPair() {
			return false
		}

	case BondRequest:
		security, ok := h.link.(SecurityLink)
		if !ok {
			log.Printf("%s: this link has no Bluetooth security channel, so no Security Request; asking for Pair here instead", h.link.Addr())
			if !h.askForPair() {
				return false
			}
		} else if !h.requestPairing(security) {
			return false
		}
	}
	h.sensor.Bonded(h.displayType)
	h.link.Notify(CharAuth, []byte{opBondConfirmed, 0x01})
	log.Printf("%s: bonded as display type %d", h.link.Addr(), h.displayType)
	return true
}

// refusedHangUp waits the 0.2 s a real sensor leaves between a refusal and hanging up.
func (h *handler) refusedHangUp() {
	select {
	case <-h.done:
	case <-time.After(bondRefuseHangUp):
	}
}

// askForPair holds the link for an operator's Pair for as long as the display's keep-alive says.
func (h *handler) askForPair() bool {
	r := h.sensor.requestBond(h.link.Addr(), h.displayType, h.holdUntil, false)
	defer h.sensor.endBondRequest(h.displayType, r)
	log.Printf("%s: display type %d's system shows the Pair prompt; waiting %v for Pair (POST /bond {\"accept\":%d})",
		h.link.Addr(), h.displayType, time.Until(h.holdUntil).Round(time.Second), h.displayType)
	select {
	case <-r.accepted:
		log.Printf("%s: Pair tapped on display type %d", h.link.Addr(), h.displayType)
		return true
	case <-time.After(time.Until(r.deadline)):
		log.Printf("%s: nobody tapped Pair on display type %d; hanging up", h.link.Addr(), h.displayType)
		return false
	case <-h.done:
		return false
	}
}

// requestPairing sends the sensor's Security Request and lets the display's own system decide,
// for as long as the display's keep-alive holds the link:
//
//   - it refuses (Pairing Failed, as an iPhone or Apple Watch does with its app not in front), or
//     ends the pairing while its prompt shows (Cancel): hang up 0.2 s later, no glucose;
//   - it sends a Pairing Request and gets the sensor's Pairing Response, on which it shows its
//     Pair prompt. Its public key then says the person tapped Pair. The emulator cannot pair, so
//     it declines the rest and goes on as bonded, on a link that is not encrypted;
//   - nobody answers the prompt, or the system never answers the request: hang up.
//
// While the prompt shows, Pair from the page or the API also counts, as at the ask level.
func (h *handler) requestPairing(security SecurityLink) bool {
	addr, display := h.link.Addr(), h.displayType
	p, err := requestPairing(security, addr)
	if err != nil {
		log.Printf("%s: the Security Request was not sent: %v", addr, err)
		return false
	}
	log.Printf("%s: Security Request sent; display type %d's own system decides (it has %v)",
		addr, display, time.Until(h.holdUntil).Round(time.Second))

	var prompt *bondRequest
	var pairHere chan struct{} // nil, so never ready, until the prompt shows
	defer func() {
		if prompt != nil {
			h.sensor.endBondRequest(display, prompt)
		}
	}()
	timeout := time.NewTimer(time.Until(h.holdUntil))
	defer timeout.Stop()
	for {
		select {
		case e := <-p.events:
			switch e.step {
			case pairingRefused:
				if prompt == nil {
					log.Printf("%s: display type %d's system refused the pairing (reason %02x, %s); hanging up",
						addr, display, e.reason, smpReason(e.reason))
				} else {
					log.Printf("%s: display type %d's system ended the pairing while its prompt showed: Cancel, or its own time limit (reason %02x, %s); hanging up",
						addr, display, e.reason, smpReason(e.reason))
				}
				h.refusedHangUp()
				return false
			case pairingPrompted:
				if prompt == nil {
					prompt = h.sensor.requestBond(addr, display, h.holdUntil, true)
					pairHere = prompt.accepted
					log.Printf("%s: display type %d's system has the sensor's Pairing Response and should be showing its Pair prompt; waiting %v for Pair there (or POST /bond {\"accept\":%d})",
						addr, display, time.Until(h.holdUntil).Round(time.Second), display)
				}
			case pairingAccepted:
				log.Printf("%s: Pair tapped on display type %d (its system sent its public key); the rest of the pairing was declined, the link stays unencrypted",
					addr, display)
				return true
			}
		case <-pairHere:
			log.Printf("%s: Pair given here for display type %d, while its own prompt showed", addr, display)
			return true
		case <-timeout.C:
			if prompt == nil {
				log.Printf("%s: display type %d's system did not answer the Security Request; hanging up", addr, display)
			} else {
				log.Printf("%s: nobody tapped Pair on display type %d; hanging up", addr, display)
			}
			return false
		case <-h.done:
			return false
		}
	}
}
