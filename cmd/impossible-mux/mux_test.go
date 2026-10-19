package main

import (
	"bufio"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type end struct {
	t    *testing.T
	conn net.Conn
	in   chan map[string]interface{}
}

func newEnd(t *testing.T, conn net.Conn) *end {
	e := &end{t: t, conn: conn, in: make(chan map[string]interface{}, 64)}
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var msg map[string]interface{}
			json.Unmarshal(sc.Bytes(), &msg)
			e.in <- msg
		}
		close(e.in)
	}()
	return e
}

func (e *end) send(msg map[string]interface{}) {
	b, _ := json.Marshal(msg)
	e.conn.Write(append(b, '\n'))
}

func (e *end) expect(typ string) map[string]interface{} {
	e.t.Helper()
	select {
	case msg, ok := <-e.in:
		if !ok {
			e.t.Fatalf("closed, want %s", typ)
		}
		if msg["type"] != typ {
			e.t.Fatalf("got %v, want %s", msg, typ)
		}
		return msg
	case <-time.After(2 * time.Second):
		e.t.Fatalf("no %s", typ)
	}
	return nil
}

func (e *end) quiet() {
	e.t.Helper()
	select {
	case msg, ok := <-e.in:
		if ok {
			e.t.Fatalf("unexpected %v", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func (e *end) closed() {
	e.t.Helper()
	select {
	case msg, ok := <-e.in:
		if ok {
			e.t.Fatalf("unexpected %v", msg)
		}
	case <-time.After(2 * time.Second):
		e.t.Fatal("still open")
	}
}

func listen(t *testing.T, path string) chan *end {
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *end, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			ch <- newEnd(t, c)
		}
	}()
	t.Cleanup(func() { l.Close() })
	return ch
}

func accepted(t *testing.T, ch chan *end) *end {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("the mux did not connect")
	}
	return nil
}

type fakeSuspender struct {
	mu       sync.Mutex
	attached int
	wakes    int
}

func (f *fakeSuspender) Attach(pid int, bundle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = pid
}

func (f *fakeSuspender) Detach(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attached == pid {
		f.attached = 0
	}
}

func (f *fakeSuspender) Wake(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attached == pid {
		f.wakes++
	}
}

func (f *fakeSuspender) state() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached, f.wakes
}

func TestMuxRoutesEachPeripheralToItsProvider(t *testing.T) {
	// Unix socket paths are short; $TMPDIR on macOS is too long for one.
	dir, err := ioutil.TempDir("/tmp", "mux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sensorPath, podPath, appPath := filepath.Join(dir, "g7.sock"), filepath.Join(dir, "pod.sock"), filepath.Join(dir, "app.sock")
	sensors, pods := listen(t, sensorPath), listen(t, podPath)

	l, err := net.Listen("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	suspended := &fakeSuspender{}
	m := newMux([]string{sensorPath, podPath}, suspended)
	go func() {
		c, err := l.Accept()
		if err == nil {
			m.serve(c)
		}
	}()
	conn, err := net.Dial("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	app := newEnd(t, conn)
	sensor, pod := accepted(t, sensors), accepted(t, pods)

	app.send(map[string]interface{}{"type": "hello", "bundleId": "org.loopkit.Loop", "pid": 1})
	sensor.expect("hello")
	pod.expect("hello")
	app.send(map[string]interface{}{"type": "scan"})
	sensor.expect("scan")
	pod.expect("scan")

	sensor.send(map[string]interface{}{"type": "didDiscover", "id": "SENSOR"})
	app.expect("didDiscover")
	pod.send(map[string]interface{}{"type": "didDiscover", "id": "POD"})
	app.expect("didDiscover")

	// Requests for a peripheral go to its provider alone.
	app.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	sensor.quiet()
	_, before := suspended.state()
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	app.expect("didConnect")
	// Every provider's events wake a suspended app, not only the sensor's.
	if attached, wakes := suspended.state(); attached != 1 || wakes != before+1 {
		t.Fatalf("app %d woken %d times for the pod's event, want app 1 woken once", attached, wakes-before)
	}

	// An advertisement wakes the app only if its scan would find it.
	app.send(map[string]interface{}{"type": "scan", "services": []string{"C00A"}})
	sensor.expect("scan")
	pod.expect("scan")
	advert := func(services ...string) map[string]interface{} {
		return map[string]interface{}{"type": "didDiscover", "id": "POD",
			"adv": map[string]interface{}{"kCBAdvDataServiceUUIDs": services}}
	}
	_, before = suspended.state()
	pod.send(advert("4024", "C001"))
	app.expect("didDiscover")
	if _, wakes := suspended.state(); wakes != before {
		t.Fatal("woken for an advertisement the scan filters out")
	}
	pod.send(advert("4024", "0000C00A-0000-1000-8000-00805F9B34FB"))
	app.expect("didDiscover")
	if _, wakes := suspended.state(); wakes != before+1 {
		t.Fatal("not woken for an advertisement the scan finds")
	}

	// A connect to a peripheral the app knows from before goes to every
	// provider; the one holding it is its owner, the others' refusals drop.
	app.send(map[string]interface{}{"type": "connect", "id": "OLD-SENSOR"})
	sensor.expect("connect")
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didFailConnect", "id": "OLD-SENSOR", "error": "unknown peripheral"})
	app.quiet()
	app.send(map[string]interface{}{"type": "discoverServices", "id": "OLD-SENSOR"})
	sensor.expect("discoverServices")
	pod.quiet()

	// A provider that goes away disconnects its peripherals from the app;
	// a restarted one sees the session from its start.
	pod.conn.Close()
	if msg := app.expect("didDisconnect"); msg["id"] != "POD" {
		t.Fatalf("disconnected %v", msg)
	}
	pod = accepted(t, pods)
	pod.expect("hello")
	pod.expect("scan")

	// The app's end is each provider's.
	conn.Close()
	sensor.closed()
	pod.closed()
	if attached, _ := suspended.state(); attached != 0 {
		t.Fatalf("still following app %d after it left", attached)
	}
}

// Two apps, a phone and a watch, share one provider. The pod has one link:
// the app that holds it talks to it, the other's connect waits until the
// holder lets go, and an app that leaves lets go of what it held.
func TestTwoAppsTakeTurnsOnOnePeripheral(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "mux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	podPath, appPath := filepath.Join(dir, "pod.sock"), filepath.Join(dir, "app.sock")
	pods := listen(t, podPath)
	l, err := net.Listen("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	suspended := &fakeSuspender{}
	m := newMux([]string{podPath}, suspended)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	dial := func() (*end, net.Conn) {
		conn, err := net.Dial("unix", appPath)
		if err != nil {
			t.Fatal(err)
		}
		return newEnd(t, conn), conn
	}
	phone, phoneConn := dial()
	pod := accepted(t, pods)
	phone.send(map[string]interface{}{"type": "hello", "bundleId": "org.loopkit.Loop", "pid": 1})
	pod.expect("hello")
	watch, watchConn := dial()
	watch.send(map[string]interface{}{"type": "hello", "bundleId": "org.loopkit.Loop.watchkitapp", "pid": 2})
	// The provider hears one client.
	pod.quiet()

	// Both scan; the provider is asked once, and both hear the pod.
	phone.send(map[string]interface{}{"type": "scan"})
	pod.expect("scan")
	watch.send(map[string]interface{}{"type": "scan"})
	pod.quiet()
	pod.send(map[string]interface{}{"type": "didDiscover", "id": "POD"})
	phone.expect("didDiscover")
	watch.expect("didDiscover")

	// The phone connects; the watch's connect waits.
	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")
	watch.quiet()
	watch.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.quiet()
	watch.quiet()

	// The holder's requests go through; the other's are refused as on a
	// disconnected peripheral.
	phone.send(map[string]interface{}{"type": "write", "id": "POD", "characteristicId": "C"})
	pod.expect("write")
	watch.send(map[string]interface{}{"type": "write", "id": "POD", "characteristicId": "C"})
	if msg := watch.expect("didWriteValue"); msg["error"] != "not connected" {
		t.Fatalf("watch's write answered %v", msg)
	}
	pod.quiet()

	// The phone lets go: the pod hears it, and then the watch's connect.
	phone.send(map[string]interface{}{"type": "cancel", "id": "POD"})
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	phone.expect("didDisconnect")
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	watch.expect("didConnect")
	phone.quiet()

	// The phone leaves; the watch keeps the pod and the provider.
	phoneConn.Close()
	watch.send(map[string]interface{}{"type": "write", "id": "POD", "characteristicId": "C"})
	pod.expect("write")
	// Its scan is gone with it; the watch still scans, so the provider still does.
	pod.quiet()

	// The watch, the last app, leaves holding the pod: the provider sees
	// its client end, as it would without the mux.
	watchConn.Close()
	pod.closed()
	if attached, _ := suspended.state(); attached != 0 {
		t.Fatalf("still following app %d after it left", attached)
	}
}

// An app that leaves while holding a link lets the next waiting app in once
// the provider reports the disconnect.
func TestAWaitingAppGetsThePeripheralWhenTheHolderLeaves(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "mux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	podPath, appPath := filepath.Join(dir, "pod.sock"), filepath.Join(dir, "app.sock")
	pods := listen(t, podPath)
	l, err := net.Listen("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := newMux([]string{podPath}, nil)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	dial := func() (*end, net.Conn) {
		conn, err := net.Dial("unix", appPath)
		if err != nil {
			t.Fatal(err)
		}
		return newEnd(t, conn), conn
	}
	phone, phoneConn := dial()
	pod := accepted(t, pods)
	phone.send(map[string]interface{}{"type": "hello", "bundleId": "org.loopkit.Loop", "pid": 1})
	pod.expect("hello")
	watch, _ := dial()
	watch.send(map[string]interface{}{"type": "hello", "bundleId": "org.loopkit.Loop.watchkitapp", "pid": 2})

	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")
	watch.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.quiet()

	// The phone dies holding the pod.
	phoneConn.Close()
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	watch.expect("didConnect")
}

// An app out of range loses its link and hears no advertisements; its
// connect waits until it is back. A provider out of range is out of range of
// every app.
func TestOutOfRange(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "mux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	podPath, appPath := filepath.Join(dir, "pod.sock"), filepath.Join(dir, "app.sock")
	pods := listen(t, podPath)
	l, err := net.Listen("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := newMux([]string{podPath}, nil)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	dial := func() *end {
		conn, err := net.Dial("unix", appPath)
		if err != nil {
			t.Fatal(err)
		}
		return newEnd(t, conn)
	}
	phone := dial()
	pod := accepted(t, pods)
	phone.send(map[string]interface{}{"type": "hello", "bundleId": "phone", "pid": 1})
	pod.expect("hello")
	watch := dial()
	watch.send(map[string]interface{}{"type": "hello", "bundleId": "watch", "pid": 2})
	phone.send(map[string]interface{}{"type": "scan"})
	pod.expect("scan")
	watch.send(map[string]interface{}{"type": "scan"})
	pod.quiet()
	pod.send(map[string]interface{}{"type": "didDiscover", "id": "POD"})
	phone.expect("didDiscover")
	watch.expect("didDiscover")
	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")

	// The phone goes out of range: its link drops with an error, the pod is
	// told, and the watch, in range, can take the pod.
	if err := m.setRange("phone", "", false); err != nil {
		t.Fatal(err)
	}
	if msg := phone.expect("didDisconnect"); msg["error"] == "" {
		t.Fatalf("out-of-range disconnect without an error: %v", msg)
	}
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	phone.quiet()
	pod.send(map[string]interface{}{"type": "didDiscover", "id": "POD"})
	watch.expect("didDiscover")
	phone.quiet()
	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.quiet()
	watch.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	watch.expect("didConnect")

	// The pod goes out of range of both: the watch's link drops, and its
	// next connect waits.
	if err := m.setRange("", "pod", false); err != nil {
		t.Fatal(err)
	}
	watch.expect("didDisconnect")
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	watch.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.quiet()

	// The pod back: the watch's pending connect goes through; the phone,
	// still away, keeps waiting.
	if err := m.setRange("", "pod", true); err != nil {
		t.Fatal(err)
	}
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	watch.expect("didConnect")
	phone.quiet()

	// The watch lets go; the phone back in range gets its connect through.
	watch.send(map[string]interface{}{"type": "cancel", "id": "POD"})
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	watch.expect("didDisconnect")
	pod.quiet()
	if err := m.setRange("phone", "", true); err != nil {
		t.Fatal(err)
	}
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")
}

// The phone away while it holds the pod; the watch takes and lets go of the pod and leaves; the
// phone back gets its connect through and then talks to the pod.
func TestPhoneBackAfterTheWatchHadThePod(t *testing.T) {
	dir, err := ioutil.TempDir("/tmp", "mux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	podPath, appPath := filepath.Join(dir, "pod.sock"), filepath.Join(dir, "app.sock")
	pods := listen(t, podPath)
	l, err := net.Listen("unix", appPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := newMux([]string{podPath}, nil)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	dial := func() (*end, net.Conn) {
		conn, err := net.Dial("unix", appPath)
		if err != nil {
			t.Fatal(err)
		}
		return newEnd(t, conn), conn
	}
	phone, _ := dial()
	pod := accepted(t, pods)
	phone.send(map[string]interface{}{"type": "hello", "bundleId": "phone", "pid": 1})
	pod.expect("hello")
	watch, watchConn := dial()
	watch.send(map[string]interface{}{"type": "hello", "bundleId": "watch", "pid": 2})
	phone.send(map[string]interface{}{"type": "scan"})
	pod.expect("scan")
	pod.send(map[string]interface{}{"type": "didDiscover", "id": "POD"})
	phone.expect("didDiscover")
	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")

	// Away: the phone's link drops; its driver dials again at once.
	m.setRange("phone", "", false)
	phone.expect("didDisconnect")
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	phone.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.quiet()

	// The watch takes the pod, lets go, and leaves.
	watch.send(map[string]interface{}{"type": "connect", "id": "POD"})
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	watch.expect("didConnect")
	watch.send(map[string]interface{}{"type": "cancel", "id": "POD"})
	pod.expect("cancel")
	pod.send(map[string]interface{}{"type": "didDisconnect", "id": "POD"})
	watch.expect("didDisconnect")
	watchConn.Close()
	pod.quiet()

	// Back: the phone's waiting connect goes through, and its requests reach the pod.
	m.setRange("phone", "", true)
	pod.expect("connect")
	pod.send(map[string]interface{}{"type": "didConnect", "id": "POD"})
	phone.expect("didConnect")
	phone.send(map[string]interface{}{"type": "discoverServices", "id": "POD"})
	pod.expect("discoverServices")
}
