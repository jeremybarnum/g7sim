// impossible-mux lets several simulated peripherals serve one app in the iOS
// Simulator. ImpossiBLE connects the app to a single provider socket; the mux
// takes that socket and fans the app out to providers on their own sockets,
// such as g7sim and the LoopKit/pod simulator:
//
//	g7sim -transport impossible -socket /tmp/impossible-g7.sock
//	pod -transport impossible -socket /tmp/impossible-pod.sock
//	impossible-mux /tmp/impossible-g7.sock /tmp/impossible-pod.sock
//
// Each provider sees one client, as it would without the mux, while several
// apps share the mux: a phone app and a watch app in a paired pair of
// Simulators, say. A peripheral has one link, as over the air: the app that
// holds it talks to it, and another app's connect waits until the holder lets
// go, as iOS keeps a connect pending. Each connect names its app, so the
// sensor treats each app as its own display.
//
// With -api, apps and providers can be put out of range and back:
//
//	curl -X POST 'localhost:8087/range?app=<bundle id>&in=0'   (the app away)
//	curl -X POST 'localhost:8087/power?app=<bundle id>&on=0'   (Bluetooth off in the app)
//	curl -X POST 'localhost:8087/range?provider=pod&in=0'      (the pod away)
//
// A link out of range drops, a connect out of range waits, and the last
// advertisement is repeated to an app that comes back or starts scanning.
// MUX_TRACE=1 logs each request and event for a peripheral, with its holder.
//
// With -suspend the mux, not each provider, stops the app while iOS would have
// it suspended, and wakes it for any provider's Bluetooth event.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/loopkit/g7sim/suspend"
)

func main() {
	socket := flag.String("socket", "/tmp/impossible.sock", "the socket ImpossiBLE connects the app to")
	simulator := flag.String("simulator", "", "the app's simulator (name or UDID), for -suspend")
	suspendApp := flag.Bool("suspend", false, "stop the app while iOS would have it suspended, waking it for Bluetooth events")
	connectDelay := flag.Duration("connect-delay", 0, "hold each app's connect this long before the provider makes it, as a radio takes time to bring a link up")
	api := flag.String("api", "", "address of the range API, e.g. :8087: GET /range; POST /range?app=<bundle id>&in=0|1 or ?provider=<part of a socket path>&in=0|1; POST /power?app=<bundle id>&on=0|1")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [-socket path] [-suspend -simulator name] provider.sock...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var s suspender
	if *suspendApp {
		if *simulator == "" {
			log.Fatalf("-suspend needs -simulator")
		}
		s = suspend.New(*simulator)
	}
	if c, err := net.Dial("unix", *socket); err == nil {
		c.Close()
		log.Fatalf("%s is served by another provider; quit it (ImpossiBLE's menu bar app?) first", *socket)
	}
	os.Remove(*socket)
	l, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s for %v", *socket, flag.Args())
	m := newMux(flag.Args(), s)
	m.connectDelay = *connectDelay
	if *api != "" {
		go serveAPI(*api, m)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go m.serve(conn)
	}
}

// serveAPI puts apps and providers out of range and back, for tests that
// need a phone away, a watch away or a pod unplugged.
func serveAPI(addr string, m *mux) {
	http.HandleFunc("/range", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			q := r.URL.Query()
			if err := m.setRange(q.Get("app"), q.Get("provider"), q.Get("in") != "0"); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.rangeState())
	})
	http.HandleFunc("/power", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := m.setPower(q.Get("app"), q.Get("on") != "0"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.rangeState())
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}
