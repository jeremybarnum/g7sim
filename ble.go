//go:build linux
// +build linux

package main

import (
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loopkit/g7sim/g7"
	"github.com/paypal/gatt"
	"github.com/paypal/gatt/linux/cmd"
)

var (
	advertisedService = gatt.UUID16(0xFEBC)
	cgmService        = gatt.MustParseUUID("F8083532-849E-531C-C594-30F1F86A4EA5")
	serviceB          = gatt.MustParseUUID("F8084532-849E-531C-C594-30F1F86A4EA5")

	charUUIDs = map[g7.Char]gatt.UUID{
		g7.CharControl:     gatt.MustParseUUID("F8083534-849E-531C-C594-30F1F86A4EA5"),
		g7.CharAuth:        gatt.MustParseUUID("F8083535-849E-531C-C594-30F1F86A4EA5"),
		g7.CharBackfill:    gatt.MustParseUUID("F8083536-849E-531C-C594-30F1F86A4EA5"),
		g7.CharCertificate: gatt.MustParseUUID("F8083538-849E-531C-C594-30F1F86A4EA5"),
	}
	communicationUUID = gatt.MustParseUUID("F8083533-849E-531C-C594-30F1F86A4EA5")
	charE             = gatt.MustParseUUID("F8084533-849E-531C-C594-30F1F86A4EA5")
	charF             = gatt.MustParseUUID("F8084534-849E-531C-C594-30F1F86A4EA5")
)

type link struct {
	central gatt.Central
	addr    string
	events  chan event
	handler g7.Handler

	mu        sync.Mutex
	notifiers map[g7.Char]gatt.Notifier
}

func (l *link) Addr() string { return l.addr }

func (l *link) Notify(c g7.Char, b []byte) error {
	l.mu.Lock()
	n := l.notifiers[c]
	l.mu.Unlock()
	if n == nil || n.Done() {
		log.Printf("%s %s -> %s dropped: not subscribed", l.addr, c, hex.EncodeToString(b))
		return errors.New("not subscribed")
	}
	log.Printf("%s %s -> %s", l.addr, c, hex.EncodeToString(b))
	_, err := n.Write(b)
	return err
}

func (l *link) Close() {
	log.Printf("%s: sensor closing the link", l.addr)
	l.central.Close()
}

func (l *link) run() {
	for e := range l.events {
		if e.subscribed {
			l.handler.Subscribed(e.char)
		} else {
			l.handler.Write(e.char, e.data)
		}
	}
	l.handler.Closed()
}

type Radio struct {
	sensor *g7.Sensor
	device gatt.Device

	mu    sync.Mutex
	links map[gatt.Central]*link
	mode  g7.AdvertMode
	types byte
}

// staticAddress derives a random static address from the serial, so each
// emulated sensor is a distinct peripheral to iOS, as real sensors are.
func staticAddress(serial uint64) [6]byte {
	var a [6]byte // little endian, as HCI wants it
	x := serial*0x9E3779B97F4A7C15 + 0x632BE59BD9B4E019
	for i := range a {
		a[i] = byte(x >> (8 * i))
	}
	a[5] |= 0xC0
	return a
}

func NewRadio(sensor *g7.Sensor, maxConnections int) (*Radio, error) {
	interval := uint16(sensor.Timing.AdvertisingInterval / (625 * time.Microsecond))
	d, err := gatt.NewDevice(
		gatt.LnxMaxConnections(maxConnections),
		gatt.LnxDeviceID(adapterID(), true),
		gatt.LnxSetAdvertisingParameters(&cmd.LESetAdvertisingParameters{
			AdvertisingIntervalMin: interval,
			AdvertisingIntervalMax: interval,
			AdvertisingType:        0x00, // connectable undirected
			OwnAddressType:         0x01, // random
			AdvertisingChannelMap:  0x7,
		}),
	)
	if err != nil {
		return nil, err
	}
	addr := staticAddress(sensor.Serial())
	if err := d.Option(gatt.LnxSendHCIRawCommand(&cmd.LESetRandomAddress{RandomAddress: addr}, nil)); err != nil {
		return nil, err
	}
	log.Printf("sensor address %02X:%02X:%02X:%02X:%02X:%02X", addr[5], addr[4], addr[3], addr[2], addr[1], addr[0])

	r := &Radio{sensor: sensor, device: d, links: map[gatt.Central]*link{}}
	d.Handle(
		gatt.CentralConnected(r.connected),
		gatt.CentralDisconnected(r.disconnected),
	)
	ready := make(chan struct{})
	err = d.Init(func(d gatt.Device, s gatt.State) {
		log.Printf("adapter %s", s)
		if s == gatt.StatePoweredOn {
			r.addServices()
			close(ready)
		}
	})
	if err != nil {
		return nil, err
	}
	<-ready
	return r, nil
}

func (r *Radio) addServices() {
	gap := gatt.NewService(gatt.UUID16(0x1800))
	// The GAP device name the app reads after connecting is the same short
	// name the sensor advertises (DXCMxx). A real G7 does not switch to a
	// "Dexcomxx" form once connected (Marion, on real hardware, 2026-10).
	gap.AddCharacteristic(gatt.UUID16(0x2A00)).SetValue([]byte(r.sensor.Name()))
	gap.AddCharacteristic(gatt.UUID16(0x2A01)).SetValue([]byte{0, 0})

	s := gatt.NewService(cgmService)
	s.AddCharacteristic(communicationUUID).HandleNotifyFunc(func(gatt.Request, gatt.Notifier) {})
	for _, c := range []g7.Char{g7.CharControl, g7.CharAuth, g7.CharBackfill, g7.CharCertificate} {
		c := c
		ch := s.AddCharacteristic(charUUIDs[c])
		ch.HandleWriteFunc(func(req gatt.Request, data []byte) byte {
			r.enqueue(req.Central, event{char: c, data: append([]byte{}, data...)})
			return 0
		})
		ch.HandleNotifyFunc(func(req gatt.Request, n gatt.Notifier) {
			if l := r.link(req.Central); l != nil {
				l.mu.Lock()
				l.notifiers[c] = n
				l.mu.Unlock()
			}
			r.enqueue(req.Central, event{char: c, subscribed: true})
		})
	}

	b := gatt.NewService(serviceB)
	for _, u := range []gatt.UUID{charE, charF} {
		ch := b.AddCharacteristic(u)
		ch.HandleWriteFunc(func(req gatt.Request, data []byte) byte {
			log.Printf("%s service B write %s", req.Central.ID(), hex.EncodeToString(data))
			return 0
		})
		ch.HandleNotifyFunc(func(gatt.Request, gatt.Notifier) {})
	}

	for _, svc := range []*gatt.Service{gap, s, b} {
		if err := r.device.AddService(svc); err != nil {
			log.Fatalf("adding service %s: %v", svc.UUID(), err)
		}
	}
}

func (r *Radio) link(c gatt.Central) *link {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.links[c]
}

func (r *Radio) enqueue(c gatt.Central, e event) {
	l := r.link(c)
	if l == nil {
		return
	}
	select {
	case l.events <- e:
	default:
		log.Printf("%s: event queue full, dropping %s write", l.addr, e.char)
	}
}

func (r *Radio) connected(c gatt.Central) {
	addr := strings.ToUpper(c.ID())
	r.mu.Lock()
	mode := r.mode
	r.mu.Unlock()

	if !r.sensor.LinkOpened(addr, time.Now(), mode) {
		c.Close()
		return
	}
	log.Printf("%s connected (advertising %s)", addr, mode)
	l := &link{central: c, addr: addr, events: make(chan event, 256), notifiers: map[g7.Char]gatt.Notifier{}}
	l.handler = g7.NewHandler(r.sensor, l, mode)
	r.mu.Lock()
	r.links[c] = l
	r.mu.Unlock()
	go l.run()
}

func (r *Radio) disconnected(c gatt.Central) {
	r.mu.Lock()
	l := r.links[c]
	delete(r.links, c)
	r.mu.Unlock()
	if l == nil {
		return
	}
	log.Printf("%s disconnected", l.addr)
	close(l.events)
	r.sensor.LinkClosed(time.Now())
}

func (r *Radio) advertisement(mode g7.AdvertMode, types byte) *gatt.AdvPacket {
	a := &gatt.AdvPacket{}
	if mode == g7.AdvertBare {
		// The minute calls carry the Flags field alone.
		a.AppendFlags(0x04)
		return a
	}
	a.AppendFlags(0x06)
	a.AppendUUIDFit([]gatt.UUID{advertisedService})
	crc := g7.SerialChecksum(r.sensor.Serial())
	a.AppendManufacturerData(0x00D0, []byte{byte(crc), byte(crc >> 8), types, 0x04})
	a.AppendName(r.sensor.Name())
	return a
}

// Run drives the advertiser from the sensor's schedule.
// Stop releases the adapter, closing every link. gatt.Device does not
// declare Stop, but the Linux device has it.
func (r *Radio) Stop() {
	d, ok := r.device.(interface{ Stop() error })
	if !ok {
		log.Printf("stopping adapter: not supported")
		return
	}
	if err := d.Stop(); err != nil {
		log.Printf("stopping adapter: %v", err)
	}
}

func (r *Radio) Run() {
	for range time.Tick(50 * time.Millisecond) {
		mode := r.sensor.Tick(time.Now())
		types := r.sensor.TypesInUse()

		r.mu.Lock()
		changed := mode != r.mode || (mode == g7.AdvertFull && types != r.types)
		r.mode, r.types = mode, types
		r.mu.Unlock()
		if !changed {
			continue
		}

		var err error
		if mode == g7.AdvertOff {
			err = r.device.StopAdvertising()
		} else {
			err = r.device.Advertise(r.advertisement(mode, types))
		}
		log.Printf("advertising %s (types in use %02x)", mode, types)
		if err != nil {
			log.Printf("advertising %s: %v", mode, err)
		}
	}
}

// adapterID is the HCI index in G7SIM_HCI, or -1 for the first free adapter. Pin it when another
// emulator shares the machine: the first-free search resets an adapter that is already up, which
// can be the other emulator's.
func adapterID() int {
	if n, err := strconv.Atoi(os.Getenv("G7SIM_HCI")); err == nil {
		return n
	}
	return -1
}
