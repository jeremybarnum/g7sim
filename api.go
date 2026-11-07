package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/loopkit/g7sim/g7"
)

// serveAPI exposes the sensor's state and lets a tester steer it:
//
//	GET  /status                 sensor, slots, current window, latest reading
//	POST /glucose  {"kind":"fixed","value":65}
//	               {"kind":"sine","value":120,"amplitude":50,"periodMinutes":240}
//	               {"kind":"walk","value":140}
//	               add "algorithmState":18 to simulate a sensor issue
//	POST /forget                 drop every display's key
//	POST /mute     {"displayType":3,"muted":true}
//	               refuse one display type (2 phone, 3 watch) every reading until
//	               unmuted or restarted
//	POST /force-quit             (-transport impossible) the app's end is a force-quit:
//	                             drop what it left lodged instead of relaunching it
//	POST /new      {"model":"stelo","days":15,"code":"1234","serial":0,
//	                "ageMinutes":120,"lifetimeHours":360,"warmupMinutes":62}
//	               replace this sensor with a new one and restart; omitted
//	               fields pick defaults
func serveAPI(addr string, sensor *g7.Sensor, replace func(*g7.State)) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(indexHTML))
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sensor.Status())
	})
	http.HandleFunc("/glucose", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a glucose mode", http.StatusMethodNotAllowed)
			return
		}
		var m g7.GlucoseMode
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Kind == "" {
			http.Error(w, "bad glucose mode", http.StatusBadRequest)
			return
		}
		sensor.SetGlucoseMode(m)
		log.Printf("glucose mode now %s", m)
		writeJSON(w, sensor.Status())
	})
	http.HandleFunc("/forget", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST to forget", http.StatusMethodNotAllowed)
			return
		}
		sensor.ForgetSlots()
		log.Printf("forgot every display's key")
		writeJSON(w, sensor.Status())
	})
	http.HandleFunc("/mute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a display type to mute", http.StatusMethodNotAllowed)
			return
		}
		var m struct {
			DisplayType byte `json:"displayType"`
			Muted       bool `json:"muted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.DisplayType == 0 {
			http.Error(w, "bad mute request", http.StatusBadRequest)
			return
		}
		sensor.SetMuted(m.DisplayType, m.Muted)
		log.Printf("display type %d muted: %v", m.DisplayType, m.Muted)
		writeJSON(w, sensor.Status())
	})
	http.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a new sensor", http.StatusMethodNotAllowed)
			return
		}
		var c sensorConfig
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad sensor config: "+err.Error(), http.StatusBadRequest)
			return
		}
		next, err := newState(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("replacing %s with a new %s sensor, code %s", sensor.Name(), next.Model, next.Code)
		writeJSON(w, map[string]interface{}{"name": g7.Models[next.Model] + next.Suffix, "code": next.Code, "serial": next.Serial})
		// Let the response go out before the restart drops the listener.
		go func() {
			time.Sleep(300 * time.Millisecond)
			replace(next)
		}()
	})
	log.Printf("API on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
