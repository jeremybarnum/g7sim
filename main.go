package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/loopkit/g7sim/g7"
	"github.com/loopkit/g7sim/suspend"
)

// sensorConfig describes a new sensor. Zero values pick defaults; the
// lifetime and warmup default to the 10- or 15-day preset.
type sensorConfig struct {
	Model         string   `json:"model"`
	Days          int      `json:"days"`
	Code          string   `json:"code"`
	Serial        uint64   `json:"serial"`
	AgeMinutes    *float64 `json:"ageMinutes"`
	LifetimeHours *float64 `json:"lifetimeHours"`
	WarmupMinutes *float64 `json:"warmupMinutes"`
}

var codePattern = regexp.MustCompile(`^[0-9]{4}$`)

func newState(c sensorConfig) (*g7.State, error) {
	if c.Model == "" {
		c.Model = "g7"
	}
	if _, ok := g7.Models[c.Model]; !ok {
		return nil, fmt.Errorf("model %q: want g7, oneplus or stelo", c.Model)
	}
	if c.Days == 0 {
		c.Days = 10
		if c.Model == "stelo" {
			c.Days = 15
		}
	}
	if _, ok := g7.Presets[c.Days]; !ok {
		return nil, fmt.Errorf("days %d: want 10 or 15", c.Days)
	}
	if c.Code == "" {
		c.Code = fmt.Sprintf("%04d", rand.Intn(10000))
	}
	if !codePattern.MatchString(c.Code) {
		return nil, fmt.Errorf("code %q: want 4 digits", c.Code)
	}
	if c.Serial == 0 {
		c.Serial = 3000000000 + uint64(rand.Int63n(999999999))
	}
	age := 2 * time.Hour
	if c.AgeMinutes != nil {
		age = minutes(*c.AgeMinutes)
	}
	if age < 0 {
		return nil, fmt.Errorf("age %v: must not be negative", age)
	}

	// Derive the two-letter name suffix from the serial, so a given serial
	// always presents as the same sensor (name and BLE address) even after
	// the state file is lost and recreated.
	suffix := fmt.Sprintf("%c%c", 'a'+byte((c.Serial/26)%26), 'a'+byte(c.Serial%26))
	s := g7.NewState(c.Serial, c.Code, c.Model, suffix, time.Now().Add(-age), c.Days)

	if c.LifetimeHours != nil {
		lifetime := time.Duration(*c.LifetimeHours * float64(time.Hour))
		if lifetime <= 0 {
			return nil, fmt.Errorf("lifetime %v: must be positive", lifetime)
		}
		s.SessionLength = lifetime + g7.GracePeriod
	}
	if c.WarmupMinutes != nil {
		s.Warmup = minutes(*c.WarmupMinutes)
	}
	// The 52 reply carries warmup as 16-bit seconds.
	if s.Warmup < 0 || s.Warmup > 0xffff*time.Second {
		return nil, fmt.Errorf("warmup %v: want 0 to 1092 minutes", s.Warmup)
	}
	if s.Warmup >= s.Lifetime() {
		return nil, fmt.Errorf("warmup %v must be shorter than the lifetime %v", s.Warmup, s.Lifetime())
	}
	return s, nil
}

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

func main() {
	statePath := flag.String("state", "g7state.json", "sensor state file")
	fresh := flag.Bool("fresh", false, "start a new sensor instead of restoring the state file")
	code := flag.String("code", "", "pairing code for a new sensor (default: random)")
	serial := flag.Uint64("serial", 0, "serial for a new sensor (default: random)")
	age := flag.Duration("age", 2*time.Hour, "how long ago a new sensor was activated; 0 starts in warmup")
	dexcomApp := flag.Bool("dexcom-app", false, "a simulated Dexcom app shares each link: a client that does not authenticate itself (stock G7SensorKit) gets readings, and apps listening for connection events hear the app connect at each reading")
	interval := flag.Duration("interval", 0, "time between readings (default: the sensor's 300 s; shorter for faster testing, the same for a sensor's whole life)")
	model := flag.String("model", "g7", "model of a new sensor: g7 (DXCM), oneplus (DX02) or stelo (DX01)")
	days := flag.Int("days", 0, "10- or 15-day preset for a new sensor (default: 15 for stelo, else 10)")
	lifetime := flag.Duration("lifetime", 0, "rated lifetime of a new sensor, before the 12 h grace period (default: from -days)")
	warmup := flag.Duration("warmup", 0, "warmup of a new sensor (default: from -days)")
	maxConn := flag.Int("max-connections", 4, "simultaneous links the controller accepts")
	api := flag.String("api", ":8080", "HTTP control API address")
	transport := flag.String("transport", "ble", "ble (a Linux Bluetooth adapter) or impossible (ImpossiBLE's socket, for an app in the iOS Simulator)")
	socket := flag.String("socket", "/tmp/impossible.sock", "ImpossiBLE socket for -transport impossible")
	simulator := flag.String("simulator", "", "the app's simulator (name or UDID), for -relaunch and -suspend")
	relaunch := flag.Bool("relaunch", false, "relaunch a terminated app for its lodged connect, as iOS does")
	suspendApp := flag.Bool("suspend", false, "stop the app while iOS would have it suspended, waking it for Bluetooth events")
	dylib := flag.String("dylib", "", "ImpossiBLE library injected into a relaunched app (default: simulator/build/libImpossiBLE.dylib beside g7sim)")
	flag.Parse()
	g7.DexcomApp = *dexcomApp
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	rand.Seed(time.Now().UnixNano())

	var state *g7.State
	if !*fresh {
		s, err := g7.LoadState(*statePath)
		if err != nil && !os.IsNotExist(err) {
			log.Fatalf("restoring %s: %v (use -fresh to start a new sensor)", *statePath, err)
		}
		state = s
	}
	if state == nil {
		ageMinutes := age.Minutes()
		c := sensorConfig{Model: *model, Days: *days, Code: *code, Serial: *serial, AgeMinutes: &ageMinutes}
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "lifetime":
				h := lifetime.Hours()
				c.LifetimeHours = &h
			case "warmup":
				m := warmup.Minutes()
				c.WarmupMinutes = &m
			}
		})
		s, err := newState(c)
		if err != nil {
			log.Fatal(err)
		}
		if err := s.Save(*statePath); err != nil {
			log.Fatalf("saving %s: %v", *statePath, err)
		}
		state = s
		log.Printf("new sensor")
	}

	log.Printf("g7sim — for development use only, not a Dexcom product; simulated data only")

	timing := g7.MeasuredTiming()
	if *interval > 0 {
		timing.ReadingInterval = *interval
		log.Printf("readings every %v", *interval)
	}
	sensor := g7.NewSensor(state, *statePath, timing)
	log.Printf("%s (%s, %d-day preset, lifetime %v, warmup %v) serial %d pairing code %s, activated %s",
		sensor.Name(), sensor.Model(), state.Days, state.Lifetime(), state.Warmup,
		sensor.Serial(), sensor.PairingCode(), state.Activation.Format(time.RFC3339))

	var radio radio
	switch *transport {
	case "ble":
		r, err := NewRadio(sensor, *maxConn)
		if err != nil {
			log.Fatalf("starting Bluetooth: %v", err)
		}
		radio = r
	case "impossible":
		r, err := NewSimRadio(sensor, *socket)
		if err != nil {
			log.Fatalf("serving %s: %v", *socket, err)
		}
		if (*relaunch || *suspendApp) && *simulator == "" {
			log.Fatalf("-relaunch and -suspend need -simulator")
		}
		if *relaunch {
			r.Relaunch = simctlRelauncher(*simulator, *dylib)
		}
		if *suspendApp {
			r.Suspend = suspend.New(*simulator)
		}
		serveForceQuit(r)
		radio = r
	default:
		log.Fatalf("transport %q: want ble or impossible", *transport)
	}

	// replace swaps in a new sensor: it is saved over the state file and the
	// emulator re-executes itself, so the new sensor starts exactly as a
	// normal launch does, with its own name, address and GATT table.
	replace := func(next *g7.State) {
		sensor.Retire()
		radio.Stop()
		if err := next.Save(*statePath); err != nil {
			log.Fatalf("saving %s: %v", *statePath, err)
		}
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("restarting: %v", err)
		}
		log.Printf("restarting as the new sensor")
		args := []string{exe, "-state", *statePath, "-api", *api, "-max-connections", strconv.Itoa(*maxConn),
			"-transport", *transport, "-socket", *socket, "-simulator", *simulator,
			"-relaunch=" + strconv.FormatBool(*relaunch), "-suspend=" + strconv.FormatBool(*suspendApp), "-dylib", *dylib}
		log.Fatalf("restarting: %v", syscall.Exec(exe, args, os.Environ()))
	}

	go serveAPI(*api, sensor, replace)
	radio.Run()
}
