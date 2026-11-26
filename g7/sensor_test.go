package g7

import (
	"encoding/hex"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestSensor(t *testing.T) (*Sensor, time.Time) {
	dir, err := ioutil.TempDir("", "g7sim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// Activated 2 h ago on a whole minute, so window 24 starts at the
	// activation plus two hours.
	activation := time.Now().Truncate(time.Minute).Add(-2 * time.Hour)
	state := NewState(123456789, "1234", "g7", "zz", activation, 10)
	s := NewSensor(state, filepath.Join(dir, "state.json"), MeasuredTiming())
	return s, activation
}

func at(start time.Time, seconds float64) time.Time {
	return start.Add(time.Duration(seconds * float64(time.Second)))
}

func expectModes(t *testing.T, s *Sensor, start time.Time, want map[float64]AdvertMode) {
	t.Helper()
	for sec, mode := range want {
		if got := s.Tick(at(start, sec)); got != mode {
			t.Errorf("at +%.1fs: %s, want %s", sec, got, mode)
		}
	}
}

func TestSerialChecksumMatchesApp(t *testing.T) {
	if got := SerialChecksum(123456789); got != 0x31C3 {
		t.Fatalf("checksum %04x, want 31c3", got)
	}
}

func TestEmptyBurst(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	// A paired sensor whose phone has been gone long enough for its slot to
	// lapse: a 25 s burst, then silent (no minute calls, nothing holds a slot).
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-20 * time.Minute)}
	expectModes(t, s, reading, map[float64]AdvertMode{
		1: AdvertOff, 2.6: AdvertFull, 27: AdvertFull, 28: AdvertOff, 61: AdvertOff,
	})
}

func TestUnpairedSensorStaysDiscoverable(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	// No phone slot yet: full advertising throughout, including between bursts.
	expectModes(t, s, reading, map[float64]AdvertMode{
		1: AdvertFull, 40: AdvertFull, 120: AdvertFull, 280: AdvertFull,
	})
}

func TestPhoneCollectsShortBurst(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-5 * time.Minute)}

	s.Tick(at(reading, 3))
	if !s.LinkOpened("AA", at(reading, 3), AdvertFull) {
		t.Fatal("phone refused")
	}
	s.Served("AA", DisplayPhone, at(reading, 4))
	s.LinkClosed(at(reading, 6))
	expectModes(t, s, reading, map[float64]AdvertMode{5: AdvertFull, 9: AdvertFull, 10: AdvertOff, 61: AdvertOff})
}

func TestServedClientIsDroppedForTheRestOfTheWindow(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.Tick(at(reading, 3))
	s.LinkOpened("AA", at(reading, 3), AdvertFull)
	s.Served("AA", DisplayPhone, at(reading, 4))
	s.LinkClosed(at(reading, 5))
	if s.LinkOpened("AA", at(reading, 8), AdvertFull) {
		t.Fatal("a served client got back in during the same window")
	}
	if s.AdmitDisplay("BB", DisplayPhone, at(reading, 8), AdvertFull) {
		t.Fatal("a second phone got in after the phone collected")
	}
	s.Tick(at(reading, 301))
	if !s.LinkOpened("AA", at(reading, 303), AdvertFull) {
		t.Fatal("served client refused in the next window")
	}
}

func TestMissingPhoneExtendsThenMinuteCalls(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-4 * time.Minute)}
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-4 * time.Minute)}

	// The watch collects; the phone does not.
	s.Tick(at(reading, 3))
	s.LinkOpened("WW", at(reading, 3), AdvertFull)
	s.Served("WW", DisplayWatch, at(reading, 4))
	s.LinkClosed(at(reading, 5))

	expectModes(t, s, reading, map[float64]AdvertMode{
		20: AdvertFull, 31: AdvertFull, 32.5: AdvertOff,
		62.6: AdvertBare, 66: AdvertOff, 182.6: AdvertBare, 242.6: AdvertBare, 246: AdvertOff,
	})
	if s.AdmitDisplay("WW", DisplayWatch, at(reading, 63), AdvertBare) {
		t.Error("watch admitted at a minute call")
	}
	if !s.AdmitDisplay("PP", DisplayPhone, at(reading, 63), AdvertBare) {
		t.Error("phone refused at a minute call")
	}
}

func TestLateWindowsAreShort(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-1 * time.Minute), Misses: 2}
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: reading}

	s.Tick(at(reading, 3))
	s.LinkOpened("WW", at(reading, 3), AdvertFull)
	s.Served("WW", DisplayWatch, at(reading, 4))
	s.LinkClosed(at(reading, 5))
	expectModes(t, s, reading, map[float64]AdvertMode{9: AdvertFull, 11: AdvertOff})
}

func TestMissesCountAcrossWindows(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading}
	s.Tick(at(reading, 3))
	s.Tick(at(reading, 303))
	s.Tick(at(reading, 603))
	if m := s.state.Slots[DisplayPhone].Misses; m != 2 {
		t.Fatalf("misses %d, want 2", m)
	}
}

func TestTypesInUse(t *testing.T) {
	s, _ := newTestSensor(t)
	s.state.Slots[DisplayPhone] = &Slot{LastSeen: time.Now()}
	s.state.Slots[DisplayWatch] = &Slot{LastSeen: time.Now()}
	s.state.Slots[DisplayMedical] = &Slot{LastSeen: time.Now().Add(-20 * time.Minute)}
	if got := s.TypesInUse(); got != 0x06 {
		t.Fatalf("types in use %02x, want 06", got)
	}
}

func TestRejectionsLockTheSensor(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.Tick(at(reading, 3))
	for i := 0; i < 4; i++ {
		s.reject(verdictMismatch, at(reading, 3))
	}
	if got := s.Tick(at(reading, 4)); got != AdvertOff {
		t.Fatalf("advertising %s while locked out", got)
	}
}

func TestReadingsAndBackfill(t *testing.T) {
	s, _ := newTestSensor(t)
	r, now := s.Latest()
	if r == nil || r.Glucose == nil || r.State != algoOK {
		t.Fatalf("latest %v", r)
	}
	if now-r.Time >= 300 {
		t.Fatalf("latest reading %ds old", now-r.Time)
	}
	got := s.Backfill(r.Time-3600, r.Time)
	if len(got) != 13 {
		t.Fatalf("an hour of backfill has %d readings, want 13", len(got))
	}
	if msg := GlucoseMessage(r, now); len(msg) != 19 || msg[0] != 0x4e {
		t.Fatalf("glucose message %x", msg)
	}
	if rec := BackfillRecord(r); len(rec) != 9 {
		t.Fatalf("backfill record %x", rec)
	}
}

func TestWarmup(t *testing.T) {
	state := NewState(1, "1234", "g7", "zz", time.Now().Add(-10*time.Minute), 10)
	s := NewSensor(state, filepath.Join(os.TempDir(), "g7sim-warmup.json"), MeasuredTiming())
	r, _ := s.Latest()
	if r.State != algoWarmup || r.Glucose != nil {
		t.Fatalf("reading in warmup: %v", r)
	}
}

func TestModelNamesAndSessionLength(t *testing.T) {
	cases := []struct {
		model, name string
		days        int
	}{
		{"g7", "DXCMzz", 10},
		{"oneplus", "DX02zz", 10},
		{"stelo", "DX01zz", 15},
		{"", "DXCMzz", 10}, // state files from before -model
	}
	for _, c := range cases {
		state := NewState(1, "1234", c.model, "zz", time.Now(), c.days)
		s := NewSensor(state, filepath.Join(t.TempDir(), "state.json"), MeasuredTiming())
		if got := s.Name(); got != c.name {
			t.Errorf("model %q: name %s, want %s", c.model, got, c.name)
		}
		if got := state.Lifetime(); got != time.Duration(c.days)*24*time.Hour {
			t.Errorf("model %q: lifetime %v, want %d days", c.model, got, c.days)
		}
	}
}

// The 52 replies must match those captured from real 10- and 15-day sensors.
func TestExtendedVersionMatchesCaptures(t *testing.T) {
	for days, want := range map[int]string{
		10: "5200c0d70d00540600020404ff0c00",
		15: "5200406f1400880e00010a04ff1100",
	} {
		got := hex.EncodeToString(ExtendedVersionMessage(NewState(1, "1234", "g7", "zz", time.Now(), days)))
		if got != want {
			t.Errorf("%d-day: %s, want %s", days, got, want)
		}
	}
}

func TestMutedDisplayIsRefusedWhileTheOthersCollect(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.Tick(at(reading, 3))
	s.SetMuted(DisplayWatch, true)
	if s.AdmitDisplay("WW", DisplayWatch, at(reading, 4), AdvertFull) {
		t.Fatal("a muted watch got in")
	}
	if !s.AdmitDisplay("PP", DisplayPhone, at(reading, 4), AdvertFull) {
		t.Fatal("the phone was refused while only the watch was muted")
	}
	s.SetMuted(DisplayWatch, false)
	if !s.AdmitDisplay("WW", DisplayWatch, at(reading, 5), AdvertFull) {
		t.Fatal("an unmuted watch was refused")
	}
}

func TestMutedWatchFindsNothingOnceThePhoneHasItsReading(t *testing.T) {
	s, act := newTestSensor(t)
	reading := act.Add(2 * time.Hour)
	s.state.Slots[DisplayPhone] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-5 * time.Minute)}
	s.state.Slots[DisplayWatch] = &Slot{Key: make([]byte, 16), LastSeen: reading.Add(-5 * time.Minute)}
	s.SetMuted(DisplayWatch, true)

	s.Tick(at(reading, 3))
	if !s.LinkOpened("PP", at(reading, 3), AdvertFull) {
		t.Fatal("phone refused")
	}
	s.Served("PP", DisplayPhone, at(reading, 4))
	expectModes(t, s, reading, map[float64]AdvertMode{4.5: AdvertOff, 9: AdvertOff, 30: AdvertOff, 61: AdvertOff})
}
