package g7

import (
	"crypto/rand"
	"fmt"
	"log"
	"sync"
	"time"
)

// Timing of the sensor's advertising, from Jeremy Barnum's sniffer and
// passive-scanner measurements of a real G7 (September 2026).
type Timing struct {
	ReadingInterval time.Duration // the grid: 300.00 s ± 1.2 s
	BurstDelay      time.Duration // first advert after the reading: +2.0…3.2 s

	// How long a burst lasts, measured from its first advert.
	ServedBurst   time.Duration // every expected display collected: ~7 s
	ExtendedBurst time.Duration // an expected display missing, its first two windows: ~29 s
	LateBurst     time.Duration // an expected display missing for longer: ~8 s
	EmptyBurst    time.Duration // nobody connected: ~25 s

	// After the last link closes the advert keeps going this long while an
	// expected display is still missing (~19 s), or briefly once all are in.
	ExtendedTailAfterClose time.Duration
	ServedTailAfterClose   time.Duration
	ExtendedWindows        int

	// While the phone misses readings, bare adverts at +60/120/180/240 s that
	// admit only the phone.
	MinuteCallLength time.Duration

	AdvertisingInterval time.Duration // ~160 ms between adverts

	SlotHold time.Duration // a quiet display keeps its slot ~15 min

	IdleDisconnect   time.Duration // the sensor closes a read link once it goes quiet
	HandshakeIdle    time.Duration
	RejectionsToLock int
	RejectionLockout time.Duration
}

func MeasuredTiming() Timing {
	return Timing{
		ReadingInterval:        300 * time.Second,
		BurstDelay:             2500 * time.Millisecond,
		ServedBurst:            7 * time.Second,
		ExtendedBurst:          29 * time.Second,
		LateBurst:              8 * time.Second,
		EmptyBurst:             25 * time.Second,
		ExtendedTailAfterClose: 19 * time.Second,
		ServedTailAfterClose:   1 * time.Second,
		ExtendedWindows:        2,
		MinuteCallLength:       3 * time.Second,
		AdvertisingInterval:    160 * time.Millisecond,
		SlotHold:               15 * time.Minute,
		IdleDisconnect:         2 * time.Second,
		HandshakeIdle:          15 * time.Second,
		RejectionsToLock:       4,
		RejectionLockout:       10 * time.Minute,
	}
}

type AdvertMode int

const (
	AdvertOff  AdvertMode = iota
	AdvertFull            // name, FEBC and manufacturer data
	AdvertBare            // flags only: the minute calls
)

func (m AdvertMode) String() string {
	return [...]string{"off", "full", "bare"}[m]
}

const (
	DisplayMedical = 1
	DisplayPhone   = 2
	DisplayWatch   = 3
)

// Slot is one display type's hold on the sensor.
type Slot struct {
	Key      []byte    `json:"key"`
	LastSeen time.Time `json:"lastSeen"`
	Misses   int       `json:"misses"`
}

type Calibration struct {
	Glucose     uint16 `json:"glucose"`
	SensorTime  uint32 `json:"sensorTime"`
	DisplayType byte   `json:"displayType"`
}

// window is what the sensor knows about the clients of one reading.
type window struct {
	index       int
	start       time.Time // first advert
	servedTypes map[byte]bool
	servedAddrs map[string]bool
	anyLink     bool
	openLinks   int
	lastClose   time.Time
}

type Sensor struct {
	mu      sync.Mutex
	Timing  Timing
	state   *State
	path    string
	glucose *GlucoseModel

	win         *window
	rejections  int
	lockedUntil time.Time
	addrTypes   map[string]byte // display type each address last authenticated as
}

func NewSensor(state *State, path string, timing Timing) *Sensor {
	if state.Identity == nil {
		state.Identity = NewSensorIdentity(state.Serial)
	}
	s := &Sensor{
		Timing:    timing,
		state:     state,
		path:      path,
		glucose:   NewGlucoseModel(state),
		addrTypes: map[string]byte{},
	}
	s.fillReadings(time.Now())
	return s
}

// CertChain is the sensor's X.509 chain for the certificate exchange.
func (s *Sensor) CertChain() [][]byte { return s.state.Identity.CertChain() }

// SignChallenge signs the display's key-challenge nonce with the sensor leaf.
func (s *Sensor) SignChallenge(displayNonce []byte) []byte {
	return s.state.Identity.SignChallenge(displayNonce)
}

func (s *Sensor) Name() string        { return Models[s.Model()] + s.state.Suffix }
func (s *Sensor) Serial() uint64      { return s.state.Serial }
func (s *Sensor) PairingCode() string { return s.state.Code }

func (s *Sensor) sensorTime(t time.Time) uint32 {
	return uint32(t.Sub(s.state.Activation) / time.Second)
}

func (s *Sensor) readingIndex(t time.Time) int {
	if t.Before(s.state.Activation) {
		return -1
	}
	return int(t.Sub(s.state.Activation) / s.Timing.ReadingInterval)
}

func (s *Sensor) readingTime(i int) time.Time {
	return s.state.Activation.Add(time.Duration(i) * s.Timing.ReadingInterval)
}

// fillReadings generates every reading up to t that does not exist yet.
func (s *Sensor) fillReadings(t time.Time) bool {
	last := s.readingIndex(t)
	added := false
	for i := s.state.nextIndex(); i <= last; i++ {
		r := s.glucose.Reading(i, s.sensorTimeOf(i), s.Timing.ReadingInterval)
		s.state.addReading(r)
		added = true
	}
	return added
}

func (s *Sensor) sensorTimeOf(i int) uint32 {
	return uint32(time.Duration(i) * s.Timing.ReadingInterval / time.Second)
}

// Tick advances readings and windows and returns the advertising the sensor
// should be doing now.
func (s *Sensor) Tick(now time.Time) AdvertMode {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fillReadings(now) {
		r := s.state.latest()
		log.Printf("reading %d: %s", r.Index, r)
		s.save()
	}

	idx := s.readingIndex(now)
	if idx < 0 {
		return AdvertOff
	}
	if s.win == nil || s.win.index != idx {
		s.closeWindow(now)
		s.win = &window{
			index:       idx,
			start:       s.readingTime(idx).Add(s.Timing.BurstDelay),
			servedTypes: map[byte]bool{},
			servedAddrs: map[string]bool{},
		}
	}
	if now.Before(s.lockedUntil) {
		return AdvertOff
	}

	// A sensor that has never paired stays fully discoverable between bursts,
	// so a phone can find it to pair at any time; a new sensor advertises
	// aggressively before its first connection. It settles into the measured
	// grid once a phone has connected.
	if s.state.Slots[DisplayPhone] == nil {
		return AdvertFull
	}

	w := s.win
	if now.Before(w.start) {
		// Between the reading and its first advert, the previous window's
		// minute calls have ended; nothing on the air.
		return AdvertOff
	}
	if now.Before(s.burstEnd(now)) {
		return AdvertFull
	}
	if s.phoneMissing(now) {
		offset := now.Sub(w.start)
		for m := 1; m <= 4; m++ {
			call := time.Duration(m) * time.Minute
			if offset >= call && offset < call+s.Timing.MinuteCallLength {
				return AdvertBare
			}
		}
	}
	return AdvertOff
}

func (s *Sensor) closeWindow(now time.Time) {
	if s.win == nil {
		return
	}
	for t, slot := range s.state.Slots {
		if s.win.servedTypes[t] {
			slot.Misses = 0
		} else if s.isHeld(slot, now) {
			slot.Misses++
			log.Printf("window %d: display type %d did not collect (%d in a row)", s.win.index, t, slot.Misses)
		}
	}
	s.save()
}

func (s *Sensor) isHeld(slot *Slot, now time.Time) bool {
	return slot != nil && now.Sub(slot.LastSeen) < s.Timing.SlotHold
}

func (s *Sensor) burstEnd(now time.Time) time.Time {
	w := s.win
	t := s.Timing
	if !w.anyLink {
		return w.start.Add(t.EmptyBurst)
	}
	lastClose := w.lastClose
	if w.openLinks > 0 {
		lastClose = now
	}
	missing, extended := false, false
	for typ, slot := range s.state.Slots {
		if s.isHeld(slot, now) && !w.servedTypes[typ] {
			missing = true
			if slot.Misses < t.ExtendedWindows {
				extended = true
			}
		}
	}
	end := func(burst, tail time.Duration) time.Time {
		a, b := w.start.Add(burst), lastClose.Add(tail)
		if b.After(a) {
			return b
		}
		return a
	}
	switch {
	case !missing:
		return end(t.ServedBurst, t.ServedTailAfterClose)
	case extended:
		return end(t.ExtendedBurst, t.ExtendedTailAfterClose)
	default:
		return end(t.LateBurst, t.ServedTailAfterClose)
	}
}

func (s *Sensor) phoneMissing(now time.Time) bool {
	slot := s.state.Slots[DisplayPhone]
	return s.isHeld(slot, now) && !s.win.servedTypes[DisplayPhone]
}

// TypesInUse is the advertisement's byte of display types holding a slot.
func (s *Sensor) TypesInUse() byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b byte
	now := time.Now()
	for t, slot := range s.state.Slots {
		if t >= 1 && t <= 8 && s.isHeld(slot, now) {
			b |= 1 << (t - 1)
		}
	}
	return b
}

// LinkOpened decides whether the controller should keep a new link. A
// client already served this window is dropped at once, as the sensor does;
// during the minute calls only the phone gets in.
func (s *Sensor) LinkOpened(addr string, now time.Time, mode AdvertMode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win == nil {
		return false
	}
	if s.win.servedAddrs[addr] {
		log.Printf("%s: already served reading %d; dropping the link", addr, s.win.index)
		return false
	}
	if mode == AdvertBare {
		if t, ok := s.addrTypes[addr]; ok && t != DisplayPhone {
			log.Printf("%s: display type %d at a minute call; only the phone is admitted", addr, t)
			return false
		}
	}
	s.win.anyLink = true
	s.win.openLinks++
	return true
}

func (s *Sensor) LinkClosed(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win == nil {
		return
	}
	if s.win.openLinks > 0 {
		s.win.openLinks--
	}
	s.win.lastClose = now
}

// AdmitDisplay is the check at the client's challenge, once its display type
// is known.
func (s *Sensor) AdmitDisplay(addr string, displayType byte, now time.Time, mode AdvertMode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrTypes[addr] = displayType
	if s.win != nil && s.win.servedTypes[displayType] {
		log.Printf("%s: display type %d already collected reading %d; dropping", addr, displayType, s.win.index)
		return false
	}
	if mode == AdvertBare && displayType != DisplayPhone {
		log.Printf("%s: display type %d at a minute call; dropping", addr, displayType)
		return false
	}
	return true
}

func (s *Sensor) SlotKey(displayType byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.state.Slots[displayType]; slot != nil {
		return slot.Key
	}
	return nil
}

type AuthVerdict struct{ Status, Detail byte }

var (
	verdictBonded         = AuthVerdict{0x01, 0x01}
	verdictPaired         = AuthVerdict{0x01, 0x00}
	verdictMismatch       = AuthVerdict{0x02, 0x01}
	verdictTypeRestricted = AuthVerdict{0x02, 0x02}
	verdictNoAppKey       = AuthVerdict{0x02, 0x03}
)

// Authenticated records a client that proved its key. newKey is set when
// the key came out of a J-PAKE exchange on this link.
func (s *Sensor) Authenticated(addr string, displayType byte, newKey []byte, now time.Time) AuthVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.state.Slots[displayType]
	if newKey != nil {
		// A completed key exchange means the client has the pairing code and
		// physical possession, so it takes the slot, replacing any earlier
		// key for this display type.
		s.state.Slots[displayType] = &Slot{Key: newKey, LastSeen: now}
		s.rejections = 0
		s.save()
		return verdictPaired
	}
	if slot == nil {
		return s.reject(verdictNoAppKey, now)
	}
	slot.LastSeen = now
	s.rejections = 0
	return verdictBonded
}

func (s *Sensor) Rejected(v AuthVerdict) AuthVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reject(v, time.Now())
}

func (s *Sensor) reject(v AuthVerdict, now time.Time) AuthVerdict {
	s.rejections++
	if s.rejections >= s.Timing.RejectionsToLock {
		s.lockedUntil = now.Add(s.Timing.RejectionLockout)
		s.rejections = 0
		log.Printf("%d rejections in a row: not accepting connections until %s", s.Timing.RejectionsToLock, s.lockedUntil.Format("15:04:05"))
	}
	return v
}

// Served marks a client as having collected the current reading.
func (s *Sensor) Served(addr string, displayType byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win == nil {
		return
	}
	s.win.servedAddrs[addr] = true
	s.win.servedTypes[displayType] = true
	if slot := s.state.Slots[displayType]; slot != nil {
		slot.LastSeen = now
		slot.Misses = 0
	}
}

// Window is the reading the sensor is advertising now, or -1 between windows.
func (s *Sensor) Window() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win == nil {
		return -1
	}
	return s.win.index
}

func (s *Sensor) Latest() (*Reading, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.fillReadings(now)
	return s.state.latest(), s.sensorTime(now)
}

func (s *Sensor) Backfill(start, end uint32) []*Reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Reading
	for _, r := range s.state.Readings {
		if r.Time >= start && r.Time <= end {
			out = append(out, r)
		}
	}
	return out
}

func (s *Sensor) Calibrate(glucose uint16, at uint32, displayType byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Calibration = &Calibration{Glucose: glucose, SensorTime: at, DisplayType: displayType}
	s.glucose.Calibrate(glucose, at)
	s.save()
}

func (s *Sensor) CalibrationBounds() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CalibrationBoundsMessage(s.state.Calibration, 1)
}

func (s *Sensor) ExtendedVersion() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ExtendedVersionMessage(s.state)
}

func (s *Sensor) Model() string {
	if _, ok := Models[s.state.Model]; ok {
		return s.state.Model
	}
	return "g7"
}

// Status is the snapshot served by the HTTP API.
func (s *Sensor) Status() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	slots := map[string]interface{}{}
	for t, slot := range s.state.Slots {
		slots[fmt.Sprint(t)] = map[string]interface{}{
			"lastSeen": slot.LastSeen, "held": s.isHeld(slot, now), "misses": slot.Misses,
		}
	}
	out := map[string]interface{}{
		"name": s.Name(), "serial": s.state.Serial, "code": s.state.Code,
		"model": s.Model(), "days": s.state.Days,
		"lifetimeHours": s.state.Lifetime().Hours(), "warmupMinutes": s.state.Warmup.Minutes(),
		"activation": s.state.Activation, "slots": slots,
		"glucoseMode": s.glucose.Describe(),
	}
	if r := s.state.latest(); r != nil {
		out["latest"] = r
	}
	if s.win != nil {
		var served []byte
		for t := range s.win.servedTypes {
			served = append(served, t)
		}
		out["window"] = map[string]interface{}{"index": s.win.index, "start": s.win.start, "servedTypes": served, "openLinks": s.win.openLinks}
	}
	if now.Before(s.lockedUntil) {
		out["lockedUntil"] = s.lockedUntil
	}
	return out
}

func (s *Sensor) SetGlucoseMode(m GlucoseMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.glucose.SetMode(m)
	s.state.Glucose = m
	s.save()
}

// ForgetSlots drops every display's key, as a sensor replaced in place would.
func (s *Sensor) ForgetSlots() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Slots = map[byte]*Slot{}
	s.save()
}

// Retire stops the sensor writing its state file, so a replacement can be
// saved there without being overwritten.
func (s *Sensor) Retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = ""
}

func (s *Sensor) save() {
	if s.path == "" {
		return
	}
	if err := s.state.Save(s.path); err != nil {
		log.Printf("saving state: %v", err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
