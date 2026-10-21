package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// line is one end of a newline-delimited JSON connection.
type line struct {
	conn net.Conn
	mu   sync.Mutex
}

func (l *line) send(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := l.conn.Write(append(append([]byte(nil), b...), '\n')); err != nil {
		log.Printf("send to %s: %v", l.conn.RemoteAddr(), err)
	}
}

func readLines(conn net.Conn, each func(raw []byte, msg map[string]interface{})) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			log.Printf("bad message: %v", err)
			continue
		}
		each(append([]byte(nil), sc.Bytes()...), msg)
	}
}

func str(msg map[string]interface{}, key string) string {
	s, _ := msg[key].(string)
	return s
}

func now() float64 { return float64(time.Now().UnixNano())/1e9 - 978307200 }

// suspender is suspend.Suspender's part the mux uses.
type suspender interface {
	Attach(pid int, bundle string)
	Detach(pid int)
	Wake(pid int)
}

// mux connects every app on ImpossiBLE's socket to every provider. Each
// provider sees one client; the apps share it. A peripheral has one link, as
// over the air: the app that holds it talks to it, and another app's connect
// waits until the holder lets go, as iOS keeps a connect pending until the
// peripheral advertises again.
type mux struct {
	providers []string
	suspend   suspender // set: each app is stopped while iOS would have it suspended

	mu       sync.Mutex
	apps     map[*app]bool
	backends []*line // one connection per provider, nil while it is down
	serving  bool    // the provider loops run while an app is connected

	hello     []byte                  // the first app's, repeated to a provider that joins late
	lastHello *app                    // the app whose launch a provider's restored state answers
	scan      []byte                  // the providers' scan, while any app scans
	owner     map[string]int          // provider of each peripheral id
	pending   map[string]map[int]bool // providers asked to connect to a peripheral no one has claimed yet
	holder    map[string]*app         // the app connecting to, or connected with, a peripheral
	connected map[string]bool         // the holder's link is up
	holdRaw   map[string][]byte       // the holder's connect, to re-queue it when it goes out of range
	waiting   map[string][]*held      // apps whose connect waits for the holder to let go
	asked     *app                    // the app whose mock configuration the providers answer

	// Out of range, set from the API: an app out of range of everything (its
	// phone's radios off, its watch away), a provider out of range of every
	// app (the pod unplugged). A link that goes out of range drops; a connect
	// out of range waits, as iOS keeps it pending; advertisements don't arrive.
	away    map[string]bool // app bundle ids
	dark    map[int]bool    // provider indices
	cutting map[string]bool // links the mux dropped, until the provider confirms

	// The last advertisement of each peripheral with no link up. A peripheral advertises all the
	// while it is unconnected; a provider reports it only when a scan starts or a link drops, so
	// the mux repeats it to an app that starts scanning or comes back in range.
	adverts map[string]advert
}

type advert struct {
	raw []byte
	msg map[string]interface{}
}

// held is a connect the mux keeps until the peripheral is free.
type held struct {
	app *app
	raw []byte
}

// app is one app's connection to the mux.
type app struct {
	line   *line
	closed bool
	pid    int
	bundle string
	scan   []byte          // its scan, while it scans
	filter map[string]bool // its service filter, canonical; empty: every advertisement

	connEvents bool // it listens for connection events: another app's link coming up
}

func newMux(providers []string, s suspender) *mux {
	return &mux{
		providers: providers,
		suspend:   s,
		apps:      map[*app]bool{},
		backends:  make([]*line, len(providers)),
		owner:     map[string]int{},
		pending:   map[string]map[int]bool{},
		holder:    map[string]*app{},
		connected: map[string]bool{},
		holdRaw:   map[string][]byte{},
		waiting:   map[string][]*held{},
		away:      map[string]bool{},
		dark:      map[int]bool{},
		cutting:   map[string]bool{},
		adverts:   map[string]advert{},
	}
}

// advertTo sends app a the advertisements it would hear now: peripherals with no link up, in
// range of it, that its scan matches. Called without the lock.
func (m *mux) advertTo(a *app) {
	m.mu.Lock()
	var out [][]byte
	if a.scan != nil && !a.closed {
		for id, ad := range m.adverts {
			if m.holder[id] != nil || m.blocked(a, id) || !a.matchesScan(ad.msg) {
				continue
			}
			out = append(out, ad.raw)
		}
	}
	m.mu.Unlock()
	for _, raw := range out {
		a.line.send(raw)
	}
}

// blocked says whether app a is out of range of peripheral id. Called with
// the lock held.
func (m *mux) blocked(a *app, id string) bool {
	if m.away[a.bundle] {
		return true
	}
	o, ok := m.owner[id]
	return ok && m.dark[o]
}

// nextWaiting takes the first waiting connect for id whose app is in range,
// and makes its app the holder. Called with the lock held.
func (m *mux) nextWaiting(id string) *held {
	for k, h := range m.waiting[id] {
		if !h.app.closed && !m.blocked(h.app, id) {
			m.waiting[id] = append(append([]*held(nil), m.waiting[id][:k]...), m.waiting[id][k+1:]...)
			m.holder[id] = h.app
			m.holdRaw[id] = h.raw
			return h
		}
	}
	return nil
}

// Range is the out-of-range state the API reads and sets.
type Range struct {
	Away []string `json:"away"` // apps out of range of everything
	Dark []string `json:"dark"` // providers out of range of every app
}

func (m *mux) rangeState() Range {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := Range{Away: []string{}, Dark: []string{}}
	for b, on := range m.away {
		if on {
			r.Away = append(r.Away, b)
		}
	}
	for i, on := range m.dark {
		if on {
			r.Dark = append(r.Dark, m.providers[i])
		}
	}
	sort.Strings(r.Away)
	return r
}

// setRange puts an app (bundle id) or a provider (a part of its socket path)
// out of range or back. Links that go out of range drop at once, as at a
// supervision timeout; connects that come back in range go through.
func (m *mux) setRange(bundle, provider string, inRange bool) error {
	m.mu.Lock()
	if provider != "" {
		found := false
		for i, p := range m.providers {
			if strings.Contains(p, provider) {
				m.dark[i] = !inRange
				found = true
			}
		}
		if !found {
			m.mu.Unlock()
			return fmt.Errorf("no provider matches %q", provider)
		}
	}
	if bundle != "" {
		m.away[bundle] = !inRange
	}
	type event struct {
		a   *app
		msg map[string]interface{}
	}
	var events []event
	var cancels []string
	for id, h := range m.holder {
		if !m.blocked(h, id) {
			continue
		}
		if m.connected[id] {
			events = append(events, event{h, map[string]interface{}{"type": "didDisconnect", "id": id,
				"error": "The connection has timed out unexpectedly.", "timestamp": now(), "isReconnecting": false}})
		} else if raw := m.holdRaw[id]; raw != nil {
			// A connect not yet through stays pending, out of range.
			m.waiting[id] = append([]*held{{h, raw}}, m.waiting[id]...)
		}
		delete(m.holder, id)
		delete(m.connected, id)
		m.cutting[id] = true
		cancels = append(cancels, id)
	}
	var dispatch []string
	if inRange {
		for id, hs := range m.waiting {
			if len(hs) > 0 && m.holder[id] == nil && !m.cutting[id] {
				dispatch = append(dispatch, id)
			}
		}
	}
	m.mu.Unlock()
	log.Printf("range: app %q provider %q in range %v", bundle, provider, inRange)
	for _, e := range events {
		m.toApp(e.a, e.msg)
	}
	for _, id := range cancels {
		m.mu.Lock()
		to := m.targets(id)
		m.mu.Unlock()
		raw, _ := json.Marshal(map[string]interface{}{"type": "cancel", "id": id})
		m.sendTo(to, raw)
		id := id
		// A provider that does not answer the cancel frees the link anyway.
		time.AfterFunc(1500*time.Millisecond, func() { m.cutDone(id) })
	}
	for _, id := range dispatch {
		m.dispatch(id)
	}
	if inRange {
		for _, a := range m.appsList() {
			m.advertTo(a)
		}
	}
	return nil
}

// cutDone ends a link the mux dropped, and lets a waiting connect in.
func (m *mux) cutDone(id string) {
	m.mu.Lock()
	if !m.cutting[id] {
		m.mu.Unlock()
		return
	}
	delete(m.cutting, id)
	m.mu.Unlock()
	m.dispatch(id)
}

// dispatch sends the next in-range waiting connect for a free peripheral.
func (m *mux) dispatch(id string) {
	m.mu.Lock()
	if m.holder[id] != nil || m.cutting[id] {
		m.mu.Unlock()
		return
	}
	next := m.nextWaiting(id)
	var to []int
	if next != nil {
		to = m.targets(id)
	}
	m.mu.Unlock()
	if next != nil {
		m.sendTo(to, next.raw)
	}
}

// serve connects an app. The providers are kept while any app is there.
func (m *mux) serve(conn net.Conn) {
	a := &app{line: &line{conn: conn}}
	m.mu.Lock()
	m.apps[a] = true
	start := !m.serving
	m.serving = true
	m.mu.Unlock()
	if start {
		for i := range m.providers {
			go m.keepProvider(i)
		}
	}
	readLines(conn, func(raw []byte, msg map[string]interface{}) { m.fromApp(a, raw, msg) })
	m.appGone(a)
}

// keepProvider connects to provider i and reconnects when it restarts, for
// as long as any app is there.
func (m *mux) keepProvider(i int) {
	path := m.providers[i]
	reported := false
	for {
		m.mu.Lock()
		serving := m.serving
		m.mu.Unlock()
		if !serving {
			return
		}
		conn, err := net.Dial("unix", path)
		if err != nil {
			if !reported {
				log.Printf("%s: %v; retrying", path, err)
				reported = true
			}
			time.Sleep(time.Second)
			continue
		}
		reported = false
		b := &line{conn: conn}
		m.mu.Lock()
		if !m.serving {
			m.mu.Unlock()
			conn.Close()
			return
		}
		m.backends[i] = b
		// A provider that joins late sees the session as one that just began.
		hello, scan := m.hello, m.scan
		m.mu.Unlock()
		log.Printf("%s: connected", path)
		if hello != nil {
			b.send(hello)
		}
		if scan != nil {
			b.send(scan)
		}
		readLines(conn, func(raw []byte, msg map[string]interface{}) { m.fromProvider(i, raw, msg) })
		m.providerGone(i)
		log.Printf("%s: disconnected", path)
	}
}

// providerGone forgets a provider that went away. Its peripherals disconnect
// from their holders, as a device that powers off does, and the connects
// waiting for them fail.
func (m *mux) providerGone(i int) {
	m.mu.Lock()
	m.backends[i] = nil
	type lost struct {
		a   *app
		msg map[string]interface{}
	}
	var out []lost
	for id, o := range m.owner {
		if o != i {
			continue
		}
		delete(m.owner, id)
		if a := m.holder[id]; a != nil {
			if m.connected[id] {
				out = append(out, lost{a, map[string]interface{}{"type": "didDisconnect", "id": id,
					"error": "provider went away", "timestamp": now(), "isReconnecting": false}})
			} else {
				out = append(out, lost{a, map[string]interface{}{"type": "didFailConnect", "id": id, "error": "provider went away"}})
			}
			delete(m.holder, id)
			delete(m.connected, id)
		}
		for _, h := range m.waiting[id] {
			out = append(out, lost{h.app, map[string]interface{}{"type": "didFailConnect", "id": id, "error": "provider went away"}})
		}
		delete(m.waiting, id)
	}
	for id, pend := range m.pending {
		if !pend[i] {
			continue
		}
		delete(pend, i)
		if len(pend) == 0 {
			delete(m.pending, id)
			if a := m.holder[id]; a != nil {
				out = append(out, lost{a, map[string]interface{}{"type": "didFailConnect", "id": id, "error": "unknown peripheral"}})
				delete(m.holder, id)
			}
		}
	}
	serving := m.serving
	m.mu.Unlock()
	if !serving {
		return
	}
	for _, l := range out {
		m.toApp(l.a, l.msg)
	}
}

// appGone forgets an app that left. The links it held are cancelled, so the
// peripherals advertise again for the others; with no app left, every
// provider sees its client end, as it would without the mux.
func (m *mux) appGone(a *app) {
	m.mu.Lock()
	if a.closed {
		m.mu.Unlock()
		return
	}
	a.closed = true
	delete(m.apps, a)
	pid := a.pid
	log.Printf("app %s (pid %d) left; %d app(s)", a.bundle, a.pid, len(m.apps))
	var cancels []struct {
		to  []int
		raw []byte
	}
	for id, h := range m.holder {
		if h != a {
			continue
		}
		raw, _ := json.Marshal(map[string]interface{}{"type": "cancel", "id": id})
		cancels = append(cancels, struct {
			to  []int
			raw []byte
		}{m.targets(id), raw})
		// The provider's didDisconnect, with no holder to take it, lets the next app in.
	}
	for id, hs := range m.waiting {
		m.waiting[id] = without(hs, a)
	}
	last := len(m.apps) == 0
	var backends []*line
	if last {
		m.serving = false
		backends, m.backends = m.backends, make([]*line, len(m.providers))
		m.hello, m.scan, m.lastHello, m.asked = nil, nil, nil, nil
		m.owner, m.pending = map[string]int{}, map[string]map[int]bool{}
		m.holder, m.connected, m.waiting = map[string]*app{}, map[string]bool{}, map[string][]*held{}
		m.holdRaw, m.cutting = map[string][]byte{}, map[string]bool{}
		m.adverts = map[string]advert{}
	}
	m.mu.Unlock()
	if m.suspend != nil && pid > 0 {
		m.suspend.Detach(pid)
	}
	a.line.conn.Close()
	if last {
		for _, b := range backends {
			if b != nil {
				b.conn.Close()
			}
		}
		return
	}
	for _, c := range cancels {
		m.sendTo(c.to, c.raw)
	}
	m.rescan()
}

func without(hs []*held, a *app) []*held {
	var out []*held
	for _, h := range hs {
		if h.app != a {
			out = append(out, h)
		}
	}
	return out
}

func (m *mux) live() []int {
	var out []int
	for i, b := range m.backends {
		if b != nil {
			out = append(out, i)
		}
	}
	return out
}

// targets names the providers a peripheral's request goes to: its owner, or
// every provider asked to find it. Called with the lock held.
func (m *mux) targets(id string) []int {
	if o, ok := m.owner[id]; ok {
		return []int{o}
	}
	var out []int
	for i := range m.pending[id] {
		out = append(out, i)
	}
	return out
}

func (m *mux) sendTo(indices []int, raw []byte) {
	m.mu.Lock()
	var targets []*line
	for _, i := range indices {
		if b := m.backends[i]; b != nil {
			targets = append(targets, b)
		}
	}
	m.mu.Unlock()
	for _, b := range targets {
		b.send(raw)
	}
}

// rescan sends the providers the apps' scans as one: every advertisement
// any app's filter would match, or no scan when no app scans. The mux
// filters each advertisement per app.
func (m *mux) rescan() {
	m.mu.Lock()
	var scan []byte
	scanning, filtered := false, true
	union := map[string]bool{}
	for a := range m.apps {
		if a.scan == nil {
			continue
		}
		scanning = true
		if len(a.filter) == 0 {
			filtered = false
		}
		for u := range a.filter {
			union[u] = true
		}
	}
	if scanning {
		msg := map[string]interface{}{"type": "scan"}
		if filtered {
			var services []string
			for u := range union {
				services = append(services, u)
			}
			sort.Strings(services)
			msg["services"] = services
		}
		scan, _ = json.Marshal(msg)
	}
	changed := string(scan) != string(m.scan)
	m.scan = scan
	all := m.live()
	m.mu.Unlock()
	if !changed {
		return
	}
	if scan == nil {
		scan, _ = json.Marshal(map[string]interface{}{"type": "stopScan"})
	}
	m.sendTo(all, scan)
}

// trace logs each request and event for a peripheral, with its holder (MUX_TRACE=1).
var trace = os.Getenv("MUX_TRACE") != ""

func (m *mux) traceLine(dir, who, t, id string) {
	if !trace || id == "" || t == "didDiscover" || t == "scan" {
		return
	}
	h := "none"
	if a := m.holder[id]; a != nil {
		h = a.bundle
	}
	log.Printf("trace %s %s %s %s holder=%s connected=%v cutting=%v waiting=%d", dir, who, t, id[:8], h, m.connected[id], m.cutting[id], len(m.waiting[id]))
}

func (m *mux) fromApp(a *app, raw []byte, msg map[string]interface{}) {
	t, id := str(msg, "type"), str(msg, "id")
	m.mu.Lock()
	m.traceLine("app>", a.bundle, t, id)
	if a.closed {
		m.mu.Unlock()
		return
	}
	all := m.live()
	var to []int
	var reply map[string]interface{} // a Bluetooth event the mux makes up
	switch {
	case t == "hello":
		a.bundle = str(msg, "bundleId")
		if pid, _ := msg["pid"].(float64); pid > 0 {
			a.pid = int(pid)
		}
		m.lastHello = a
		log.Printf("app %s (pid %d) connected; %d app(s)", a.bundle, a.pid, len(m.apps))
		// Each provider hears one client: the first app introduces it.
		if m.hello == nil {
			m.hello = raw
			to = all
		}
		pid, bundle := a.pid, a.bundle
		m.mu.Unlock()
		if m.suspend != nil && pid > 0 {
			m.suspend.Attach(pid, bundle)
		}
		m.sendTo(to, raw)
		return
	case t == "scan":
		a.scan = raw
		a.filter = map[string]bool{}
		if services, ok := msg["services"].([]interface{}); ok {
			for _, v := range services {
				if u, ok := v.(string); ok {
					a.filter[canonicalUUID(u)] = true
				}
			}
		}
		m.mu.Unlock()
		m.rescan()
		m.advertTo(a)
		return
	case t == "stopScan":
		a.scan, a.filter = nil, nil
		m.mu.Unlock()
		m.rescan()
		return
	case t == "registerForConnectionEvents":
		a.connEvents = true
		to = all
	case t == "setMockConfiguration" || t == "clearMockConfiguration":
		m.asked = a
		to = all
	case id == "":
		to = all
	case t == "connect":
		// Name the app, so a provider that tells centrals apart (the
		// sensor's per-reading bookkeeping) sees each app as its own.
		if a.bundle != "" {
			msg["central"] = a.bundle
			raw, _ = json.Marshal(msg)
		}
		switch h := m.holder[id]; {
		case (h == nil || h == a) && (m.cutting[id] || m.blocked(a, id)):
			// Out of range, or the link the mux dropped not yet gone: pending.
			if h == a {
				delete(m.holder, id)
			}
			m.waiting[id] = append(without(m.waiting[id], a), &held{a, raw})
			m.mu.Unlock()
			return
		case h != nil && h != a:
			// Held until the holder lets go, as iOS holds a connect until the
			// peripheral advertises again.
			m.waiting[id] = append(without(m.waiting[id], a), &held{a, raw})
			m.mu.Unlock()
			return
		case h == a:
			to = m.targets(id)
		default:
			m.holder[id] = a
			m.holdRaw[id] = raw
			delete(m.connected, id)
			to = m.targets(id)
			if len(to) == 0 && len(all) > 0 {
				// The app knows this peripheral from before (retrieved, or
				// restored); the provider that has it holds the connect and
				// the others refuse it.
				pend := map[int]bool{}
				for _, i := range all {
					pend[i] = true
				}
				m.pending[id] = pend
				to = all
			}
		}
	case t == "cancel":
		if m.holder[id] == a {
			to = m.targets(id)
		} else if len(m.waiting[id]) != len(without(m.waiting[id], a)) {
			m.waiting[id] = without(m.waiting[id], a)
			reply = map[string]interface{}{"type": "didDisconnect", "id": id, "error": "", "timestamp": now(), "isReconnecting": false}
		}
	default:
		// The holder's requests go to the provider, which answers for the
		// link's state; another app's are refused here.
		if m.holder[id] == a {
			to = m.targets(id)
		} else {
			reply = notConnected(t, msg)
		}
	}
	m.mu.Unlock()
	if reply != nil {
		if trace {
			log.Printf("trace mux> %s %s %v", a.bundle, str(reply, "type"), reply["error"])
		}
		m.toApp(a, reply)
		return
	}
	if len(to) == 0 && id != "" {
		switch t {
		case "connect":
			m.mu.Lock()
			if m.holder[id] == a {
				delete(m.holder, id)
			}
			m.mu.Unlock()
			m.toApp(a, map[string]interface{}{"type": "didFailConnect", "id": id, "error": "unknown peripheral"})
		case "cancel":
			m.toApp(a, map[string]interface{}{"type": "didDisconnect", "id": id, "error": "", "timestamp": now(), "isReconnecting": false})
		default:
			log.Printf("app: %s for unknown peripheral %s dropped", t, id)
		}
		return
	}
	m.sendTo(to, raw)
}

// notConnected answers a request on a peripheral the app does not hold, as
// CoreBluetooth answers a request on a disconnected peripheral.
func notConnected(t string, msg map[string]interface{}) map[string]interface{} {
	id := str(msg, "id")
	reply := map[string]interface{}{"id": id, "error": "not connected"}
	switch t {
	case "discoverServices":
		reply["type"] = "didDiscoverServices"
	case "discoverCharacteristics":
		reply["type"], reply["serviceId"] = "didDiscoverCharacteristics", str(msg, "serviceId")
	case "discoverDescriptors":
		reply["type"], reply["characteristicId"] = "didDiscoverDescriptors", str(msg, "characteristicId")
	case "read":
		reply["type"], reply["characteristicId"] = "didUpdateValue", str(msg, "characteristicId")
	case "write":
		reply["type"], reply["characteristicId"] = "didWriteValue", str(msg, "characteristicId")
	case "setNotify":
		reply["type"], reply["characteristicId"] = "didUpdateNotification", str(msg, "characteristicId")
	case "readRSSI":
		reply["type"] = "didReadRSSI"
	default:
		log.Printf("app: %s for %s, which it does not hold, dropped", t, id)
		return nil
	}
	return reply
}

func (m *mux) fromProvider(i int, raw []byte, msg map[string]interface{}) {
	t, id := str(msg, "type"), str(msg, "id")
	m.mu.Lock()
	m.traceLine("<prov", m.providers[i], t, id)
	var to []*app
	var quietly []*app // apps that get the message without being woken
	var next *held     // a waiting connect the peripheral is now free for
	dispatchAfter := false
	switch {
	case t == "connectionRejected":
		log.Printf("%s refused the mux: %s", m.providers[i], str(msg, "message"))
	case t == "didSetMockConfiguration":
		// Every provider answers; the app asked once.
		if live := m.live(); len(live) > 0 && live[0] == i && m.asked != nil {
			to = []*app{m.asked}
		}
	case t == "didRestoreState":
		if ids, ok := msg["peripheralIds"].([]interface{}); ok {
			for _, v := range ids {
				if p, ok := v.(string); ok {
					m.owner[p] = i
				}
			}
		}
		if m.lastHello != nil {
			to = []*app{m.lastHello}
		}
	case t == "connectionEvent":
		// Another app's link (the Dexcom app's, simulated) came up: iOS tells every app
		// listening for connection events, in range of the peripheral, whoever holds it.
		if _, ok := m.owner[id]; !ok && id != "" {
			m.owner[id] = i
		}
		for a := range m.apps {
			if a.connEvents && !a.closed && !m.blocked(a, id) {
				to = append(to, a)
			}
		}
	case t == "didDiscover":
		if _, ok := m.owner[id]; !ok && id != "" {
			m.owner[id] = i
		}
		if id != "" {
			m.adverts[id] = advert{raw, msg}
		}
		// The providers filter for every app at once; each app still gets
		// every advertisement, as with one provider. iOS wakes an app for
		// an advertisement only if it matches the app's own scan.
		for a := range m.apps {
			switch {
			case a.scan == nil, m.blocked(a, id):
			case a.matchesScan(msg):
				to = append(to, a)
			default:
				quietly = append(quietly, a)
			}
		}
	case id != "":
		forward := true
		if pend := m.pending[id]; pend[i] {
			if t == "didFailConnect" {
				delete(pend, i)
				switch len(pend) {
				case 0:
					delete(m.pending, id)
				case 1:
					// The one left is holding the connect: it has the peripheral.
					for o := range pend {
						m.owner[id] = o
					}
					delete(m.pending, id)
					forward = false
				default:
					forward = false
				}
			} else {
				m.owner[id] = i
				delete(m.pending, id)
			}
		} else if o, ok := m.owner[id]; ok && o != i {
			forward = false
			log.Printf("%s: %s for %s, which %s has, dropped", m.providers[i], t, id, m.providers[o])
		} else if !ok {
			m.owner[id] = i
		}
		if forward && m.cutting[id] && (t == "didDisconnect" || t == "didFailConnect") {
			// The end of a link the mux dropped: its app has heard already.
			delete(m.cutting, id)
			forward = false
			dispatchAfter = true
		}
		if forward {
			h := m.holder[id]
			switch t {
			case "didConnect":
				m.connected[id] = true
				delete(m.adverts, id)
			case "didDisconnect", "didFailConnect":
				delete(m.holder, id)
				delete(m.connected, id)
				next = m.nextWaiting(id)
			}
			if h != nil && !h.closed {
				to = []*app{h}
			} else if t != "didDisconnect" && t != "didFailConnect" {
				log.Printf("%s: %s for %s, which no app holds, dropped", m.providers[i], t, id)
			}
		}
	default:
		to = m.appsList()
	}
	var targets []int
	if next != nil {
		targets = m.targets(id)
	}
	m.mu.Unlock()
	for _, a := range to {
		m.wake(a)
		a.line.send(raw)
	}
	for _, a := range quietly {
		a.line.send(raw)
	}
	if next != nil {
		m.sendTo(targets, next.raw)
	}
	if dispatchAfter {
		m.dispatch(id)
	}
}

func (m *mux) appsList() []*app {
	var out []*app
	for a := range m.apps {
		out = append(out, a)
	}
	return out
}

// wake gives a suspended app a Bluetooth event's run time, as bluetoothd
// does.
func (m *mux) wake(a *app) {
	if m.suspend != nil && a.pid > 0 {
		m.suspend.Wake(a.pid)
	}
}

// toApp sends an app a Bluetooth event the mux makes up for a provider.
func (m *mux) toApp(a *app, msg map[string]interface{}) {
	b, _ := json.Marshal(msg)
	m.wake(a)
	a.line.send(b)
}

func (a *app) matchesScan(msg map[string]interface{}) bool {
	if a.scan == nil {
		return false
	}
	if len(a.filter) == 0 {
		return true
	}
	adv, _ := msg["adv"].(map[string]interface{})
	services, _ := adv["kCBAdvDataServiceUUIDs"].([]interface{})
	for _, v := range services {
		if u, ok := v.(string); ok && a.filter[canonicalUUID(u)] {
			return true
		}
	}
	return false
}

// canonicalUUID writes a 16- or 32-bit UUID in its 128-bit form, as
// CoreBluetooth compares them.
func canonicalUUID(u string) string {
	u = strings.ToUpper(u)
	switch len(u) {
	case 4:
		return "0000" + u + "-0000-1000-8000-00805F9B34FB"
	case 8:
		return u + "-0000-1000-8000-00805F9B34FB"
	}
	return u
}
