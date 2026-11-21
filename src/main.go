// main.go: entrypoint. Binds an ephemeral port, registers with the env,
// answers /healthz, then long-polls /done until the suite finishes.
//
// Env vars:
//   MAKEFUN_ENV_URL     env base URL, e.g. http://127.0.0.1:PORT
//   MAKEFUN_SHOP_TOKEN  Bearer token the clients use (empty = accept any)
//   MAKEFUN_FORGE_LOGIN viewer login (default "adaiversen", from the kit's
//                       deterministic scenario in sdk/conformance/wire.ts)
//   FORGE_PORT          fixed port for local testing (default: ephemeral)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"message":"Method Not Allowed"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"ok":true}`)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	// NOTE (round 5): debug.SetGCPercent(400) was tried here and REGRESSED
	// the read/write rounds in the local grade (fewer but longer GC pauses
	// land inside the sequential rounds); keep the default GC pacing.
	viewer := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if viewer == "" {
		viewer = "adaiversen"
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	srv := &server{store: newStore(viewer), token: token}

	addr := "127.0.0.1:0"
	if p := os.Getenv("FORGE_PORT"); p != "" {
		addr = "127.0.0.1:" + p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	log.Printf("forge: viewer=%s listening on 127.0.0.1:%d", viewer, port)

	// Round 6: the net/http server + ServeMux are replaced by the
	// hand-rolled TCP server in tcpserver.go (same handlers, same routes,
	// byte-identical responses). Routing lives in tcpServer.dispatch.
	go serveTCP(ln, srv)

	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		// Standalone mode (local development): serve forever.
		log.Printf("forge: no MAKEFUN_ENV_URL set; serving standalone on 127.0.0.1:%d", port)
		select {}
	}

	if err := register(envURL, port); err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("forge: registered with env %s", envURL)
	pollDone(envURL)
	log.Printf("forge: done; exiting 0")
	os.Exit(0)
}

// register POSTs {"port": N} to the env, retrying until the 20 s boot
// budget is nearly spent.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(18 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("register answered HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

// pollDone long-polls GET /done until the env answers {"done": true}.
func pollDone(envURL string) {
	client := &http.Client{Timeout: 0} // long-poll: no client timeout
	for {
		resp, err := client.Get(envURL + "/done")
		if err != nil {
			log.Printf("done poll error: %v; retrying", err)
			time.Sleep(time.Second)
			continue
		}
		var body struct {
			Done bool `json:"done"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.Done {
			return
		}
		// The env may answer {"done": false} before the suite ends; poll again.
		time.Sleep(200 * time.Millisecond)
	}
}
