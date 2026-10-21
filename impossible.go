package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loopkit/g7sim/g7"
	"github.com/loopkit/g7sim/suspend"
)

// SimRadio serves the sensor to an app in the iOS Simulator through
// ImpossiBLE (github.com/mickeyl/ImpossiBLE) instead of a Bluetooth adapter.
// The app links ImpossiBLE, whose CoreBluetooth shims speak newline-delimited
// JSON over a Unix socket; g7sim takes the provider's end of that socket, in
// place of ImpossiBLE's menu bar app. Like that app it serves one client, and
// a new connection takes over from the old one.
type SimRadio struct {
	sensor   *g7.Sensor
	path     string
	listener net.Listener
	id       string // the peripheral identifier the app sees
	stopped  bool

	mu       sync.Mutex
	client   *simClient
	scanning bool
	pending  bool   // the app asked to connect; held until the sensor advertises
	central  string // the address the pending connect comes from
	app      string // the app the pending connect comes from

	connEvents bool // the app listens for connection events (another app's link coming up)
	appWindow  int  // the reading the simulated Dexcom app last connected for
	link       *simLink
	mode       g7.AdvertMode
	tried      time.Time // last attempt to bring up a pending connection
	startAt    time.Time // a pending connect's start delay ends

	missedWakes int // readings advertised with nothing lodged to wake the app
	held        *heldRequests
	forceQuit   bool // the running app's end is a force-quit, not a termination

	// Relaunch starts a terminated app again, as iOS does when a request it
	// is holding for the app completes. Nil only logs that it would.
	Relaunch func(bundleID string) error

	// Suspend, if set, stops the app while iOS would have it suspended.
	Suspend *suspend.Suspender
}

// heldRequests is what an app that iOS terminated left lodged. bluetoothd
// keeps them, relaunches the app when one completes and hands them back
// through state restoration. A force-quit drops them.
type heldRequests struct {
	bundleID string
	connect  bool
	scan     bool
	launched time.Time
}

type simClient struct {
	conn    net.Conn
	addr    string // what the sensor sees as the central's address
	bundle  string
	pid     int
	restore *heldRequests // handed back at the app's first request
	wake    func()        // runs before each send: a suspended app wakes for it
	mu      sync.Mutex
	closed  bool
}

func (c *simClient) send(msg map[string]interface{}) {
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("simulator: encoding %v: %v", msg["type"], err)
		return
	}
	if c.wake != nil {
		c.wake()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		log.Printf("simulator: send %v: %v", msg["type"], err)
	}
}

func (c *simClient) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.conn.Close()
}

// gattChar is one characteristic of the table the app discovers. Those with
// a protocol role go to the handler; the rest only acknowledge.
type gattChar struct {
	uuid  string
	props int
	char  g7.Char
	role  bool
}

type gattService struct {
	uuid  string
	chars []gattChar
}

const (
	propWriteNoResponse = 0x04
	propWrite           = 0x08
	propNotify          = 0x10
)

// The same layout as ble.go serves. iOS hides the GAP service from apps.
var simServices = []gattService{
	{strings.ToUpper(cgmServiceUUID), []gattChar{
		{"F8083533-849E-531C-C594-30F1F86A4EA5", propNotify, 0, false},
		{"F8083534-849E-531C-C594-30F1F86A4EA5", propWrite | propWriteNoResponse | propNotify, g7.CharControl, true},
		{"F8083535-849E-531C-C594-30F1F86A4EA5", propWrite | propWriteNoResponse | propNotify, g7.CharAuth, true},
		{"F8083536-849E-531C-C594-30F1F86A4EA5", propWrite | propWriteNoResponse | propNotify, g7.CharBackfill, true},
		{"F8083538-849E-531C-C594-30F1F86A4EA5", propWrite | propWriteNoResponse | propNotify, g7.CharCertificate, true},
	}},
	{"F8084532-849E-531C-C594-30F1F86A4EA5", []gattChar{
		{"F8084533-849E-531C-C594-30F1F86A4EA5", propWrite | propNotify, 0, false},
		{"F8084534-849E-531C-C594-30F1F86A4EA5", propWrite | propNotify, 0, false},
	}},
}

const (
	cgmServiceUUID        = "F8083532-849E-531C-C594-30F1F86A4EA5"
	advertisedServiceUUID = "FEBC"
)

// peripheralID derives the identifier from the serial, so the app finds the
// same peripheral again after either side restarts, as it would a real one.
func peripheralID(serial uint64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("g7sim %d", serial)))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	x := strings.ToUpper(hex.EncodeToString(h[:16]))
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// centralAddr stands in for the phone's Bluetooth address, which the sensor
// keys its per-window bookkeeping on. One per app, stable across launches.
func centralAddr(bundleID string) string {
	h := sha256.Sum256([]byte(bundleID))
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", h[0]|0xC0, h[1], h[2], h[3], h[4], h[5])
}

func NewSimRadio(sensor *g7.Sensor, path string) (*SimRadio, error) {
	// A live provider (ImpossiBLE's app, or another g7sim) answers on the
	// socket; a stale file from one that died does not.
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is served by another provider; quit it (ImpossiBLE's menu bar app?) first", path)
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	r := &SimRadio{sensor: sensor, path: path, listener: l, id: peripheralID(sensor.Serial()), appWindow: -1}
	log.Printf("serving the iOS Simulator on %s as peripheral %s", path, r.id)
	go r.accept()
	return r, nil
}

func (r *SimRadio) accept() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			r.mu.Lock()
			stopped := r.stopped
			r.mu.Unlock()
			if !stopped {
				log.Printf("simulator: accept: %v", err)
			}
			return
		}
		go r.serve(&simClient{conn: conn})
	}
}

func (r *SimRadio) serve(c *simClient) {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			log.Printf("simulator: bad message: %v", err)
			continue
		}
		r.handle(c, msg)
	}
	r.dropClient(c, true)
}

// dropClient forgets an app that went away: its link closes and its scan
// stops. An app that ended, rather than lost the socket to another app, is
// one iOS terminated, and what it had lodged is held for it.
func (r *SimRadio) dropClient(c *simClient, ended bool) {
	r.mu.Lock()
	var held *heldRequests
	if r.client == c {
		if ended && !r.forceQuit && c.bundle != "" && (r.pending || r.scanning) {
			held = &heldRequests{bundleID: c.bundle, connect: r.pending, scan: r.scanning}
			r.held = held
		}
		r.forceQuit = false
		r.client = nil
		r.scanning = false
		r.pending = false
	}
	l := r.link
	if l != nil && l.client == c {
		r.link = nil
	} else {
		l = nil
	}
	r.mu.Unlock()
	if held != nil {
		log.Printf("simulator: %s ended with %s lodged; holding it as iOS would for a terminated app", c.bundle, held)
	}
	if r.Suspend != nil && c.pid > 0 {
		r.Suspend.Detach(c.pid)
	}
	if l != nil {
		l.end(false)
	}
	c.close()
}

func (h *heldRequests) String() string {
	switch {
	case h.connect && h.scan:
		return "a connect and a scan"
	case h.connect:
		return "a connect"
	default:
		return "a scan"
	}
}

func str(msg map[string]interface{}, key string) string {
	s, _ := msg[key].(string)
	return s
}

func startDelay(msg map[string]interface{}) time.Duration {
	options, _ := msg["options"].(map[string]interface{})
	seconds, _ := options["kCBConnectOptionStartDelay"].(float64)
	return time.Duration(seconds * float64(time.Second))
}

func (r *SimRadio) current(c *simClient) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client == c
}

// takeOver makes c the app served, once it has said hello: a connection
// that never does is a probe (another provider checking for a live socket).
func (r *SimRadio) takeOver(c *simClient) {
	r.mu.Lock()
	old := r.client
	r.client = c
	r.mu.Unlock()
	if old != nil {
		log.Printf("simulator: a new app connected; dropping the previous one")
		old.send(map[string]interface{}{"type": "connectionRejected", "code": "clientBusy",
			"message": "another app connected to g7sim"})
		r.dropClient(old, false)
	}
}

// restoreState hands a relaunched app the requests held for it, as iOS does
// through centralManager(_:willRestoreState:). It waits for the app's first
// request: ImpossiBLE says hello before it has recorded the central's queue.
func (r *SimRadio) restoreState(c *simClient) {
	h := c.restore
	c.restore = nil
	ids, services := []string{}, []string{}
	if h.connect {
		ids = append(ids, r.id)
	}
	if h.scan {
		services = append(services, advertisedServiceUUID)
	}
	c.send(map[string]interface{}{"type": "didRestoreState", "peripheralIds": ids,
		"scanServices": services, "scanOptions": map[string]interface{}{}})
	r.mu.Lock()
	r.pending = r.pending || h.connect
	r.scanning = r.scanning || h.scan
	r.mu.Unlock()
	log.Printf("simulator: restored %s for %s", h, c.bundle)
}

// ForceQuit drops what a terminated app left lodged, as iOS does when the
// user swipes the app away: nothing will relaunch it.
// Called while the app runs, it applies to the app's end.
func (r *SimRadio) ForceQuit() {
	r.mu.Lock()
	h := r.held
	r.held = nil
	r.forceQuit = r.client != nil
	r.mu.Unlock()
	if h != nil {
		log.Printf("simulator: %s force-quit; dropped its held %s", h.bundleID, h)
	}
}

func (r *SimRadio) handle(c *simClient, msg map[string]interface{}) {
	t := str(msg, "type")
	if t == "hello" {
		c.bundle = str(msg, "bundleId")
		c.addr = centralAddr(c.bundle)
		pid, _ := msg["pid"].(float64)
		c.pid = int(pid)
		log.Printf("simulator: %s (ImpossiBLE %s, pid %v) connected as %s",
			c.bundle, str(msg, "clientVersion"), msg["pid"], c.addr)
		if s := r.Suspend; s != nil && c.pid > 0 {
			s.Attach(c.pid, c.bundle)
			c.wake = func() { s.Wake(c.pid) }
		}
		r.takeOver(c)
		r.mu.Lock()
		if r.held != nil && r.held.bundleID == c.bundle {
			c.restore = r.held
			r.held = nil
		}
		r.mu.Unlock()
		return
	}
	if !r.current(c) {
		return
	}
	if c.restore != nil {
		r.restoreState(c)
	}
	id := str(msg, "id")
	switch t {
	case "scan":
		r.mu.Lock()
		r.scanning = true
		mode := r.mode
		r.mu.Unlock()
		if mode == g7.AdvertFull {
			r.discovered(c)
		}
	case "stopScan":
		r.mu.Lock()
		r.scanning = false
		r.mu.Unlock()
	case "connect":
		if id != r.id {
			c.send(map[string]interface{}{"type": "didFailConnect", "id": id, "error": "unknown peripheral"})
			return
		}
		r.mu.Lock()
		if r.link == nil {
			r.pending = true
			// A mux serving several apps names the one asking, so each app is
			// its own central to the sensor, as a phone and a watch are.
			r.central, r.app = c.addr, c.bundle
			if app := str(msg, "central"); app != "" {
				r.central, r.app = centralAddr(app), app
			}
			r.tried = time.Time{}
			r.startAt = time.Now().Add(startDelay(msg))
		}
		r.mu.Unlock()
		r.tryConnect(time.Now())
	case "cancel":
		r.mu.Lock()
		r.pending = false
		l := r.link
		r.link = nil
		r.mu.Unlock()
		if l != nil {
			l.end(true)
		} else {
			c.send(disconnected(id))
		}
	case "discoverServices":
		var services []map[string]interface{}
		for i, s := range simServices {
			services = append(services, map[string]interface{}{
				"id": fmt.Sprintf("%s:%s:%d", r.id, s.uuid, i), "uuid": s.uuid, "primary": true})
		}
		c.send(map[string]interface{}{"type": "didDiscoverServices", "id": id, "services": services, "error": ""})
	case "discoverCharacteristics":
		serviceID := str(msg, "serviceId")
		var chars []map[string]interface{}
		for si, s := range simServices {
			if serviceID != fmt.Sprintf("%s:%s:%d", r.id, s.uuid, si) {
				continue
			}
			for ci, ch := range s.chars {
				chars = append(chars, map[string]interface{}{
					"id": fmt.Sprintf("%s:%s:%d", serviceID, ch.uuid, ci), "uuid": ch.uuid, "properties": ch.props})
			}
		}
		c.send(map[string]interface{}{"type": "didDiscoverCharacteristics", "id": id,
			"serviceId": serviceID, "characteristics": chars, "error": ""})
	case "discoverDescriptors":
		c.send(map[string]interface{}{"type": "didDiscoverDescriptors", "id": id,
			"characteristicId": str(msg, "characteristicId"), "descriptors": []interface{}{}, "error": ""})
	case "discoverIncludedServices":
		c.send(map[string]interface{}{"type": "didDiscoverIncludedServices", "id": id,
			"serviceId": str(msg, "serviceId"), "includedServices": []interface{}{}, "error": ""})
	case "read":
		c.send(map[string]interface{}{"type": "didUpdateValue", "id": id,
			"characteristicId": str(msg, "characteristicId"), "value": "", "error": ""})
	case "write":
		r.write(c, msg)
	case "setNotify":
		r.setNotify(c, msg)
	case "readRSSI":
		c.send(map[string]interface{}{"type": "didReadRSSI", "id": id, "rssi": -60, "error": ""})
	case "registerForConnectionEvents":
		r.mu.Lock()
		r.connEvents = true
		r.mu.Unlock()
	case "setMockConfiguration":
		c.send(map[string]interface{}{"type": "didSetMockConfiguration", "ok": false,
			"error": "g7sim serves its own sensor and takes no mock configuration"})
	case "clearMockConfiguration":
	case "openL2CAP":
		c.send(map[string]interface{}{"type": "didOpenL2CAP", "id": id, "channelId": "", "psm": msg["psm"],
			"error": "g7sim has no L2CAP channels"})
	default:
		log.Printf("simulator: ignoring %s", t)
	}
}

func disconnected(id string) map[string]interface{} {
	return map[string]interface{}{"type": "didDisconnect", "id": id, "error": "",
		"timestamp": float64(time.Now().UnixNano())/1e9 - 978307200, "isReconnecting": false}
}

// discovered reports the advertisement as a scan sees it.
func (r *SimRadio) discovered(c *simClient) {
	crc := g7.SerialChecksum(r.sensor.Serial())
	mfg := []byte{0xD0, 0x00, byte(crc), byte(crc >> 8), r.sensor.TypesInUse(), 0x04}
	name := r.sensor.Name()
	c.send(map[string]interface{}{"type": "didDiscover", "id": r.id, "name": name, "rssi": -60,
		"adv": map[string]interface{}{
			"kCBAdvDataLocalName":        name,
			"kCBAdvDataIsConnectable":    true,
			"kCBAdvDataServiceUUIDs":     []string{advertisedServiceUUID},
			"kCBAdvDataManufacturerData": base64.StdEncoding.EncodeToString(mfg),
		}})
}

func (r *SimRadio) charFor(charID string) (gattChar, bool) {
	for si, s := range simServices {
		prefix := fmt.Sprintf("%s:%s:%d:", r.id, s.uuid, si)
		if !strings.HasPrefix(charID, prefix) {
			continue
		}
		for ci, ch := range s.chars {
			if charID == fmt.Sprintf("%s%s:%d", prefix, ch.uuid, ci) {
				return ch, true
			}
		}
	}
	return gattChar{}, false
}

func (r *SimRadio) activeLink(c *simClient) *simLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.link != nil && r.link.client == c {
		return r.link
	}
	return nil
}

func (r *SimRadio) write(c *simClient, msg map[string]interface{}) {
	id, charID := str(msg, "id"), str(msg, "characteristicId")
	data, err := base64.StdEncoding.DecodeString(str(msg, "value"))
	ch, ok := r.charFor(charID)
	l := r.activeLink(c)
	reply := map[string]interface{}{"type": "didWriteValue", "id": id, "characteristicId": charID, "error": ""}
	switch {
	case err != nil:
		reply["error"] = "bad value"
	case !ok:
		reply["error"] = "unknown characteristic"
	case l == nil:
		reply["error"] = "not connected"
	}
	if wt, _ := msg["writeType"].(float64); wt == 0 {
		c.send(reply)
	}
	if reply["error"] != "" {
		return
	}
	if !ch.role {
		log.Printf("%s service B write %s", l.addr, hex.EncodeToString(data))
		return
	}
	l.enqueue(event{char: ch.char, data: data})
}

func (r *SimRadio) setNotify(c *simClient, msg map[string]interface{}) {
	id, charID := str(msg, "id"), str(msg, "characteristicId")
	enabled, _ := msg["enabled"].(bool)
	if n, ok := msg["enabled"].(float64); ok {
		enabled = n != 0
	}
	ch, ok := r.charFor(charID)
	l := r.activeLink(c)
	reply := map[string]interface{}{"type": "didUpdateNotification", "id": id, "characteristicId": charID,
		"enabled": enabled, "error": ""}
	if !ok {
		reply["error"] = "unknown characteristic"
	} else if l == nil {
		reply["error"] = "not connected"
	}
	if reply["error"] != "" {
		reply["enabled"] = false
		c.send(reply)
		return
	}
	if ch.role {
		l.mu.Lock()
		if enabled {
			l.notifying[ch.char] = charID
		} else {
			delete(l.notifying, ch.char)
		}
		l.mu.Unlock()
	}
	c.send(reply)
	if ch.role && enabled {
		l.enqueue(event{char: ch.char, subscribed: true})
	}
}

// tryConnect brings up a pending connection once the sensor advertises, as
// iOS completes a pending connect on the next advert it hears after the
// connect's start delay. A link the sensor refuses stays pending, as a
// refusal at the link layer does.
func (r *SimRadio) tryConnect(now time.Time) {
	r.mu.Lock()
	c, mode, addr, app := r.client, r.mode, r.central, r.app
	if !r.pending || c == nil || r.link != nil || mode == g7.AdvertOff || now.Sub(r.tried) < time.Second || now.Before(r.startAt) {
		r.mu.Unlock()
		return
	}
	r.tried = now
	r.mu.Unlock()

	if !r.sensor.LinkOpened(addr, now, mode) {
		return
	}
	l := &simLink{radio: r, client: c, addr: addr, app: app, events: make(chan event, 256), notifying: map[g7.Char]string{}}
	r.mu.Lock()
	if r.client != c || !r.pending {
		r.mu.Unlock()
		r.sensor.LinkClosed(now)
		return
	}
	r.pending = false
	r.link = l
	r.mu.Unlock()

	log.Printf("%s connected (advertising %s)", l.addr, mode)
	c.send(map[string]interface{}{"type": "didConnect", "id": r.id})
	l.handler = g7.NewHandler(r.sensor, l, mode)
	go l.run()
}

func (r *SimRadio) Run() {
	for range time.Tick(50 * time.Millisecond) {
		now := time.Now()
		mode := r.sensor.Tick(now)
		r.mu.Lock()
		changed := mode != r.mode
		r.mode = mode
		c, scanning, connEvents := r.client, r.scanning, r.connEvents
		missed := 0
		var relaunch *heldRequests
		if changed {
			r.tried = time.Time{}
			if mode == g7.AdvertFull && !r.pending && !scanning && r.link == nil && r.held == nil {
				r.missedWakes++
				missed = r.missedWakes
			}
			if mode != g7.AdvertOff && c == nil && r.held != nil && now.Sub(r.held.launched) > 20*time.Second {
				r.held.launched = now
				relaunch = r.held
			}
		}
		r.mu.Unlock()
		if changed {
			log.Printf("advertising %s (types in use %02x)", mode, r.sensor.TypesInUse())
			if missed > 0 {
				log.Printf("NO WAKE PATH: the sensor is advertising a reading and the app has no pending connect or scan; a backgrounded Loop would sleep through it (%d so far)", missed)
			}
			if relaunch != nil {
				r.relaunch(relaunch)
			}
			if mode == g7.AdvertFull && scanning && c != nil {
				r.discovered(c)
			}
		}
		// The Dexcom app connects once for each reading: an app listening for connection events
		// hears the link come up, and may join it.
		if w := r.sensor.Window(); g7.DexcomApp && connEvents && c != nil && mode != g7.AdvertOff && w >= 0 && w != r.appWindow {
			r.appWindow = w
			log.Printf("the Dexcom app connects for reading %d: connection event to the listening apps", w)
			c.send(map[string]interface{}{"type": "connectionEvent", "id": r.id, "event": 1, "name": r.sensor.Name()})
		}
		r.tryConnect(now)
	}
}

func (r *SimRadio) relaunch(h *heldRequests) {
	if r.Relaunch == nil {
		log.Printf("simulator: iOS would relaunch %s now for its held %s (start g7sim with -simulator and -relaunch to do it)", h.bundleID, h)
		return
	}
	log.Printf("simulator: relaunching %s for its held %s", h.bundleID, h)
	go func() {
		if err := r.Relaunch(h.bundleID); err != nil {
			log.Printf("simulator: relaunching %s: %v", h.bundleID, err)
		}
	}()
}

// Stop closes the socket, ending any link; the app sees Bluetooth power off
// until a provider serves the socket again.
func (r *SimRadio) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.listener.Close()
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c != nil {
		r.dropClient(c, false)
	}
	os.Remove(r.path)
}

// simLink is the app's connection, as the handler sees it.
type simLink struct {
	radio   *SimRadio
	client  *simClient
	addr    string
	app     string // the app at the other end, as the mux names it
	events  chan event
	handler g7.Handler

	mu        sync.Mutex
	notifying map[g7.Char]string // characteristic id by char
	ended     bool
}

func (l *simLink) Addr() string { return l.addr }

// AppDisplay is the Dexcom app the link rides: the watch's for a watch app, else the phone's.
func (l *simLink) AppDisplay() byte {
	if strings.Contains(strings.ToLower(l.app), "watch") {
		return g7.DisplayWatch
	}
	return g7.DisplayPhone
}

func (l *simLink) Notify(c g7.Char, b []byte) error {
	l.mu.Lock()
	charID, ok := l.notifying[c]
	ended := l.ended
	l.mu.Unlock()
	if ended {
		return errors.New("link closed")
	}
	if !ok {
		log.Printf("%s %s -> %s dropped: not subscribed", l.addr, c, hex.EncodeToString(b))
		return errors.New("not subscribed")
	}
	log.Printf("%s %s -> %s", l.addr, c, hex.EncodeToString(b))
	l.client.send(map[string]interface{}{"type": "didUpdateValue", "id": l.radio.id,
		"characteristicId": charID, "value": base64.StdEncoding.EncodeToString(b), "error": ""})
	return nil
}

// Close is the sensor hanging up.
func (l *simLink) Close() {
	l.mu.Lock()
	ended := l.ended
	l.mu.Unlock()
	if ended {
		return
	}
	r := l.radio
	r.mu.Lock()
	if r.link == l {
		r.link = nil
	}
	r.mu.Unlock()
	log.Printf("%s: sensor closing the link", l.addr)
	l.end(true)
}

func (l *simLink) enqueue(e event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	select {
	case l.events <- e:
	default:
		log.Printf("%s: event queue full, dropping %s write", l.addr, e.char)
	}
}

// end tears the link down once, telling the app if it is still there.
func (l *simLink) end(tell bool) {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	close(l.events)
	l.mu.Unlock()
	log.Printf("%s disconnected", l.addr)
	l.radio.sensor.LinkClosed(time.Now())
	if tell {
		l.client.send(disconnected(l.radio.id))
	}
}

func (l *simLink) run() {
	for e := range l.events {
		if e.subscribed {
			l.handler.Subscribed(e.char)
		} else {
			l.handler.Write(e.char, e.data)
		}
	}
	l.handler.Closed()
}

// simctlRelauncher launches the app on a simulator with ImpossiBLE injected.
// simctl brings it to the foreground; iOS would relaunch it in the background.
func simctlRelauncher(simulator, dylib string) func(string) error {
	if dylib == "" {
		exe, err := os.Executable()
		if err == nil {
			dylib = filepath.Join(filepath.Dir(exe), "simulator", "build", "libImpossiBLE.dylib")
		}
	}
	return func(bundleID string) error {
		cmd := exec.Command("xcrun", "simctl", "launch", simulator, bundleID)
		cmd.Env = append(os.Environ(), "SIMCTL_CHILD_DYLD_INSERT_LIBRARIES="+dylib)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func serveForceQuit(r *SimRadio) {
	http.HandleFunc("/force-quit", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST to force-quit", http.StatusMethodNotAllowed)
			return
		}
		r.ForceQuit()
		w.WriteHeader(http.StatusNoContent)
	})
}
