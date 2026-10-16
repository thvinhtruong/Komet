// fakecloudapi imitates the RunPod REST API v2 for one pod, so the remote
// session lifecycle (start, wait, ssh discovery, stop, self-stop from the
// watchdog) can be tested without an account. The pod's "SSH" points at a
// local sshd given by --ssh-host/--ssh-port.
//
// It deliberately returns 404 for GET /v2/pods/{id} so the client's
// list-and-filter fallback is exercised.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	port := flag.Int("port", 18600, "")
	podID := flag.String("pod", "pod-test-1", "")
	sshHost := flag.String("ssh-host", "127.0.0.1", "")
	sshPort := flag.Int("ssh-port", 2222, "")
	bootMS := flag.Int("boot-ms", 1500, "time from start to RUNNING")
	key := flag.String("key", "rp-test-key", "expected bearer key")
	actionsLog := flag.String("actions-log", "", "append received actions here")
	flag.Parse()

	var mu sync.Mutex
	status := "EXITED"
	var readyAt time.Time

	cur := func() string {
		if status == "STARTING" && time.Now().After(readyAt) {
			status = "RUNNING"
		}
		return status
	}
	podJSON := func() map[string]any {
		st := cur()
		p := map[string]any{
			"id": *podID, "name": "komet-test", "status": st,
			"gpu":  map[string]any{"id": "NVIDIA GeForce RTX 4090", "count": 1},
			"cost": 0.0,
		}
		if st == "RUNNING" {
			p["cost"] = 0.44
			p["ssh"] = map[string]any{"direct": map[string]any{"host": *sshHost, "port": *sshPort}}
			p["runtime"] = map[string]any{"ports": []any{map[string]any{"ip": *sshHost, "private": 22, "public": *sshPort, "type": "tcp"}}}
		}
		return p
	}

	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+*key {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/v2/pods", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"pods": []any{podJSON()}, "pagination": map[string]any{"nextCursor": ""}})
	})
	mux.HandleFunc("/v2/pods/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/pods/")
		id, action, _ := strings.Cut(rest, "/")
		if id != *podID {
			http.NotFound(w, r)
			return
		}
		if action != "action" || r.Method != http.MethodPost {
			http.NotFound(w, r) // single-pod GET intentionally unsupported
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if *actionsLog != "" {
			if f, err := os.OpenFile(*actionsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), body.Action)
				f.Close()
			}
		}
		switch body.Action {
		case "start":
			if cur() == "RUNNING" {
				http.Error(w, `{"error":"pod already running"}`, http.StatusConflict)
				return
			}
			status, readyAt = "STARTING", time.Now().Add(time.Duration(*bootMS)*time.Millisecond)
		case "stop":
			status = "EXITED"
		default:
			http.Error(w, `{"error":"unsupported action"}`, http.StatusBadRequest)
			return
		}
		log.Printf("action %s -> %s", body.Action, status)
		json.NewEncoder(w).Encode(podJSON())
	})
	log.Printf("fake RunPod API on :%d (pod %s, ssh %s:%d)", *port, *podID, *sshHost, *sshPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), mux))
}
