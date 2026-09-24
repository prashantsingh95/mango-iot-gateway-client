package main

// ---------- Customer forwarding management API ----------
//
// Localhost :8090 endpoints + mqtt `forwarding.*` remote commands (platform
// UI drives these). Passwords are accepted on write, never returned on read.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func registerForwardingRoutes(mux *http.ServeMux, hs *healthServer) {
	mux.HandleFunc("/api/forwarding/status", hs.fwdStatusHandler)
	mux.HandleFunc("/api/forwarding/destinations", hs.fwdDestinationsHandler)
	mux.HandleFunc("/api/forwarding/destinations/", hs.fwdDestinationHandler)
	mux.HandleFunc("/api/forwarding/queue", hs.fwdQueueHandler)
}

func timeNowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func fwdStoreReady(w http.ResponseWriter) bool {
	if fwdDB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "forwarding store not open"})
		return false
	}
	return true
}

func (hs *healthServer) fwdStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	if !fwdStoreReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, forwardingStatus())
}

func (hs *healthServer) fwdQueueHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	if !fwdStoreReady(w) {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"queue": fwdQueueList(r.URL.Query().Get("status"), limit)})
}

func (hs *healthServer) fwdDestinationsHandler(w http.ResponseWriter, r *http.Request) {
	if !fwdStoreReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		dests, err := fwdListDestinations()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if dests == nil {
			dests = []Destination{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"destinations": dests})
	case http.MethodPost:
		var body struct {
			Destination
			Password string `json:"password"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		d := body.Destination
		if d.Enabled == false && body.Name != "" {
			// explicit false respected; default new destinations to enabled
		}
		id, err := fwdCreateDestination(&d, body.Password)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		d.ID = id
		d.Password = ""
		writeJSON(w, http.StatusCreated, d)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST only"})
	}
}

func fwdDestID(r *http.Request) (int64, string) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/forwarding/destinations/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, ""
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	return id, action
}

func (hs *healthServer) fwdDestinationHandler(w http.ResponseWriter, r *http.Request) {
	if !fwdStoreReady(w) {
		return
	}
	id, action := fwdDestID(r)
	if id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid destination id"})
		return
	}
	switch {
	case r.Method == http.MethodPut && action == "":
		var body struct {
			Destination
			Password *string `json:"password,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		d := body.Destination
		d.ID = id
		pw := body.Password
		if pw != nil && *pw == "" {
			pw = nil // empty = keep existing password
		}
		if err := fwdUpdateDestination(id, &d, pw); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"updated": true})
	case r.Method == http.MethodDelete && action == "":
		if err := fwdDeleteDestination(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
	case r.Method == http.MethodPost && action == "test":
		dests, err := fwdListDestinations()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		var found *Destination
		for i, d := range dests {
			if d.ID == id {
				found = &dests[i]
				break
			}
		}
		if found == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "destination not found"})
			return
		}
		writeJSON(w, http.StatusOK, testDestination(*found))
	case r.Method == http.MethodPost && (action == "enable" || action == "disable"):
		dests, err := fwdListDestinations()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		var found *Destination
		for i, d := range dests {
			if d.ID == id {
				found = &dests[i]
				break
			}
		}
		if found == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "destination not found"})
			return
		}
		found.Enabled = action == "enable"
		if err := fwdUpdateDestination(id, found, nil); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if action == "disable" {
			dropMqttFwdClient(id)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": found.Enabled})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "unsupported method/action"})
	}
}

// ---------- remote commands ----------

func execForwarding(cmd CommandRequest) CommandResponse {
	if fwdDB == nil {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "forwarding store not open", Timestamp: timeNowRFC3339()}
	}
	mk := func(result interface{}, err error) CommandResponse {
		if err != nil {
			return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: timeNowRFC3339()}
		}
		return CommandResponse{ID: cmd.ID, Status: "completed", Result: result, Timestamp: timeNowRFC3339()}
	}
	switch cmd.Type {
	case "forwarding.status":
		return mk(forwardingStatus(), nil)
	case "forwarding.destinations":
		dests, err := fwdListDestinations()
		if err != nil {
			return mk(nil, err)
		}
		if dests == nil {
			dests = []Destination{}
		}
		return mk(map[string]interface{}{"destinations": dests}, nil)
	case "forwarding.create":
		var body struct {
			Destination
			Password string `json:"password"`
		}
		if err := json.Unmarshal(cmd.Payload, &body); err != nil {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "invalid payload", Timestamp: timeNowRFC3339()}
		}
		d := body.Destination
		id, err := fwdCreateDestination(&d, body.Password)
		return mk(map[string]interface{}{"id": id}, err)
	case "forwarding.update":
		var body struct {
			ID int64 `json:"id"`
			Destination
			Password *string `json:"password,omitempty"`
		}
		if err := json.Unmarshal(cmd.Payload, &body); err != nil || body.ID == 0 {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "id and fields required", Timestamp: timeNowRFC3339()}
		}
		d := body.Destination
		pw := body.Password
		if pw != nil && *pw == "" {
			pw = nil
		}
		return mk(map[string]bool{"updated": true}, fwdUpdateDestination(body.ID, &d, pw))
	case "forwarding.delete":
		var body struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(cmd.Payload, &body); err != nil || body.ID == 0 {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "id required", Timestamp: timeNowRFC3339()}
		}
		dropMqttFwdClient(body.ID)
		return mk(map[string]bool{"deleted": true}, fwdDeleteDestination(body.ID))
	case "forwarding.test":
		var body struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(cmd.Payload, &body); err != nil || body.ID == 0 {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "id required", Timestamp: timeNowRFC3339()}
		}
		dests, err := fwdListDestinations()
		if err != nil {
			return mk(nil, err)
		}
		for _, d := range dests {
			if d.ID == body.ID {
				return mk(testDestination(d), nil)
			}
		}
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "destination not found", Timestamp: timeNowRFC3339()}
	case "forwarding.queue":
		var body struct {
			Status string `json:"status"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal(cmd.Payload, &body)
		return mk(map[string]interface{}{"queue": fwdQueueList(body.Status, body.Limit)}, nil)
	default:
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "unknown forwarding command: " + cmd.Type, Timestamp: timeNowRFC3339()}
	}
}
