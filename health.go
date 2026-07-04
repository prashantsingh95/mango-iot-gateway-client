package main

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

)

// ---------- HTTP Health Server ----------

type healthServer struct {
	server *http.Server
}

func newHealthServer(addr string) *healthServer {
	mux := http.NewServeMux()
	hs := &healthServer{
		server: &http.Server{Addr: addr, Handler: mux},
	}
	mux.HandleFunc("/health", hs.healthHandler)
	mux.HandleFunc("/ready", hs.readyHandler)
	return hs
}

func (hs *healthServer) start() {
	ln, err := net.Listen("tcp", hs.server.Addr)
	if err != nil {
		logger.WithError(err).Warn("health server: cannot listen, skipping")
		return
	}
	go hs.server.Serve(ln)
	logger.WithField("addr", hs.server.Addr).Info("health server started")
}

func (hs *healthServer) stop() {
	if hs.server != nil {
		hs.server.Close()
	}
}

func (hs *healthServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"uptime":  int64(time.Since(startTime).Seconds()),
		"version": version,
	})
}

func (hs *healthServer) readyHandler(w http.ResponseWriter, r *http.Request) {
	if isConnected() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
	}
}
