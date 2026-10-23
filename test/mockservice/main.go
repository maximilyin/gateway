// Command mockservice is the internal service behind the gateway in the end-to-end
// test. The test client calls /control directly to choose what proxied requests do.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type server struct {
	mu   sync.Mutex
	mode string
	hits int
}

func main() {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	s := &server{mode: "ok"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /hits", s.hitsHandler)
	mux.HandleFunc("POST /control", s.control)
	mux.HandleFunc("/", s.handle)
	log.Printf("mockservice listening on %s", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) control(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode      string `json:"mode"`
		ResetHits bool   `json:"reset_hits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Mode != "ok" && body.Mode != "fail" {
		http.Error(w, "mode must be ok or fail", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.mode = body.Mode
	if body.ResetHits {
		s.hits = 0
	}
	s.mu.Unlock()
	log.Printf("mode=%s reset_hits=%v", body.Mode, body.ResetHits)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) hitsHandler(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	hits := s.hits
	s.mu.Unlock()
	writeJSON(w, map[string]int{"hits": hits})
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits++
	mode := s.mode
	s.mu.Unlock()

	if mode == "fail" {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("fail\n"))
		return
	}
	writeJSON(w, map[string]string{
		"path":       r.URL.Path,
		"session_id": r.Header.Get("X-Session-ID"),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
