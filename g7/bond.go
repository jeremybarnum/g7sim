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
// This emulator has no Bluetooth pairing layer, so it plays the display's system's part:
// BondInstant bonds at once (unattended test runs), BondRefuse is the system refusing, and
// BondAsk holds the link for an operator's Pair, as a person must tap it on the device.

type BondMode int

const (
	BondInstant BondMode = iota // the display's system pairs at once
	BondRefuse                  // the display's system refuses: hang up, no glucose
	BondAsk                     // wait for an operator's Pair, then bond; hang up if none comes
)

var bondModeNames = [...]string{"off", "refuse", "ask"}

func (m BondMode) String() string {
	if int(m) < len(bondModeNames) {
		return bondModeNames[m]
	}
	return fmt.Sprint(int(m))
}

// ParseBondMode reads the -bond flag and the API's mode: off, refuse or ask.
func ParseBondMode(s string) (BondMode, error) {
	for i, n := range bondModeNames {
		if s == n {
			return BondMode(i), nil
		}
	}
	return 0, fmt.Errorf("bond mode %q: want off, refuse or ask", s)
}

var (
	bondRefuseHangUp = 200 * time.Millisecond // the sensor hangs up after a refused security request
	bondPromptLimit  = 31 * time.Second       // and after an unanswered Pair prompt
)

type bondRequest struct {
	addr     string
	deadline time.Time
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
// operator accepts it.
func (s *Sensor) requestBond(addr string, displayType byte, now time.Time) *bondRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &bondRequest{addr: addr, deadline: now.Add(bondPromptLimit), accepted: make(chan struct{})}
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
		out = append(out, map[string]interface{}{"displayType": t, "addr": r.addr, "secondsLeft": int(left)})
	}
	return out
}

// bond plays the display's system at the bond request, after the sensor's 07 00. It returns
// whether the link is now bonded.
func (h *handler) bond() bool {
	switch h.sensor.BondMode() {
	case BondRefuse:
		log.Printf("%s: display type %d's system refused the Bluetooth pairing; hanging up", h.link.Addr(), h.displayType)
		select {
		case <-h.done:
		case <-time.After(bondRefuseHangUp):
		}
		return false

	case BondAsk:
		r := h.sensor.requestBond(h.link.Addr(), h.displayType, time.Now())
		defer h.sensor.endBondRequest(h.displayType, r)
		log.Printf("%s: display type %d's system shows the Pair prompt; waiting up to %v for Pair (POST /bond {\"accept\":%d})",
			h.link.Addr(), h.displayType, bondPromptLimit, h.displayType)
		select {
		case <-r.accepted:
			log.Printf("%s: Pair tapped on display type %d", h.link.Addr(), h.displayType)
		case <-time.After(time.Until(r.deadline)):
			log.Printf("%s: nobody tapped Pair on display type %d; hanging up", h.link.Addr(), h.displayType)
			return false
		case <-h.done:
			return false
		}
	}
	h.sensor.Bonded(h.displayType)
	h.link.Notify(CharAuth, []byte{opBondConfirmed, 0x01})
	log.Printf("%s: bonded as display type %d", h.link.Addr(), h.displayType)
	return true
}
