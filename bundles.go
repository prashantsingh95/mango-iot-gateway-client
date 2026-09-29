package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------- apply_bundle ----------

type bundlePayload struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Payload struct {
		Type              string `json:"type"`
		Content           string `json:"content"`
		URL               string `json:"url"`
		FirmwareReleaseID string `json:"firmwareReleaseId"`
		EdgeAppID         string `json:"edgeAppId"`
	} `json:"payload"`
}

type applyBundleRequest struct {
	RolloutID string          `json:"rolloutId"`
	Bundles   []bundlePayload `json:"bundles"`
}

// execApplyBundle stores each bundle under /opt/gateway/applied/<rollout>/.
// inline scripts/configs are written to disk (scripts made executable);
// url payloads are downloaded (≤8MB, 60s) with optional sha256 verification;
// firmware/edge-app references are recorded for the matching command flows.
func execApplyBundle(cmd CommandRequest) CommandResponse {
	resp := CommandResponse{ID: cmd.ID, Timestamp: nowRFC3339()}
	var req applyBundleRequest
	if err := json.Unmarshal(cmd.Payload, &req); err != nil {
		resp.Status = "rejected"
		resp.Error = "invalid apply_bundle payload"
		return resp
	}
	if len(req.Bundles) == 0 || len(req.Bundles) > 10 {
		resp.Status = "rejected"
		resp.Error = "bundle list must contain 1-10 bundles"
		return resp
	}
	rollout := sanitizeFilename(req.RolloutID)
	if rollout == "" {
		rollout = "adhoc"
	}
	base := filepath.Join("/opt/gateway/applied", rollout)
	if err := os.MkdirAll(base, 0o755); err != nil {
		resp.Status = "failed"
		resp.Error = fmt.Sprintf("cannot stage bundles: %v", err)
		return resp
	}
	results := make([]map[string]interface{}, 0, len(req.Bundles))
	for _, b := range req.Bundles {
		results = append(results, applyOneBundle(base, b))
	}
	ok := true
	for _, r := range results {
		if applied, _ := r["applied"].(bool); !applied {
			ok = false
		}
	}
	resp.Status = "completed"
	resp.Success = ok
	resp.Result = map[string]interface{}{"rolloutId": req.RolloutID, "bundles": results}
	if !ok {
		resp.Error = "one or more bundles failed (see results)"
	}
	return resp
}

func applyOneBundle(base string, b bundlePayload) map[string]interface{} {
	out := map[string]interface{}{"id": b.ID, "name": b.Name, "applied": false}
	name := sanitizeFilename(b.Name)
	if name == "" {
		name = sanitizeFilename(b.ID)
	}
	if name == "" {
		out["error"] = "bundle has no usable name"
		return out
	}
	switch b.Payload.Type {
	case "inline":
		path := filepath.Join(base, name)
		if err := os.WriteFile(path, []byte(b.Payload.Content), 0o644); err != nil {
			out["error"] = err.Error()
			return out
		}
		if b.Kind == "script" {
			_ = os.Chmod(path, 0o755)
		}
		out["applied"] = true
		out["path"] = path
	case "url":
		if b.Payload.URL == "" || (!strings.HasPrefix(b.Payload.URL, "https://") && !strings.HasPrefix(b.Payload.URL, "http://")) {
			out["error"] = "url payload needs an http(s) url"
			return out
		}
		path := filepath.Join(base, name)
		if err := downloadBundleFile(b.Payload.URL, path, 8<<20, 60*time.Second); err != nil {
			out["error"] = err.Error()
			return out
		}
		if b.SHA256 != "" {
			sum, err := sha256File(path)
			if err != nil || !strings.EqualFold(sum, b.SHA256) {
				_ = os.Remove(path)
				out["error"] = "sha256 mismatch"
				return out
			}
		}
		out["applied"] = true
		out["path"] = path
	case "firmware":
		out["applied"] = true
		out["note"] = "firmware reference recorded — run update_firmware to flash"
	case "edge-app":
		out["applied"] = true
		out["note"] = "edge-app reference recorded — run deploy_edge_app to launch"
	default:
		out["error"] = fmt.Sprintf("unknown payload type: %s", b.Payload.Type)
	}
	return out
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func downloadBundleFile(url, path string, maxBytes int64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if n > maxBytes {
		_ = os.Remove(path)
		return fmt.Errorf("download exceeds %d bytes", maxBytes)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- deploy_edge_app ----------

type deployEdgeAppRequest struct {
	AppID   string `json:"appId"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Runtime string `json:"runtime"`
	Spec    struct {
		Image   string `json:"image"`
		Compose string `json:"compose"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	} `json:"spec"`
}

// execDeployEdgeApp launches a docker image. Anything else fails honestly:
// compose/binary runtimes need an agent upgrade, and docker must exist.
func execDeployEdgeApp(cmd CommandRequest) CommandResponse {
	resp := CommandResponse{ID: cmd.ID, Timestamp: nowRFC3339()}
	var req deployEdgeAppRequest
	if err := json.Unmarshal(cmd.Payload, &req); err != nil {
		resp.Status = "rejected"
		resp.Error = "invalid deploy_edge_app payload"
		return resp
	}
	if req.Runtime != "" && req.Runtime != "docker" {
		resp.Status = "failed"
		resp.Error = fmt.Sprintf("runtime %q not supported by this agent version (docker only)", req.Runtime)
		return resp
	}
	if req.Spec.Image == "" {
		resp.Status = "failed"
		resp.Error = "edge app has no docker image in spec"
		return resp
	}
	if !dockerAvailable() {
		resp.Status = "failed"
		resp.Error = "docker unavailable on this device"
		return resp
	}
	cname := "mango-app-" + sanitizeFilename(req.AppID)
	if cname == "mango-app-" {
		cname = "mango-app-" + sanitizeFilename(req.Name)
	}
	_, _ = dockerRun(context.Background(), "rm", "-f", cname)
	if out, err := dockerRun(context.Background(), "pull", req.Spec.Image); err != nil {
		resp.Status = "failed"
		resp.Error = fmt.Sprintf("docker pull failed: %v: %s", err, truncate(out, 300))
		return resp
	}
	args := []string{"run", "-d", "--name", cname, "--restart", "unless-stopped"}
	if req.Version != "" {
		args = append(args, "-e", "APP_VERSION="+req.Version)
	}
	args = append(args, req.Spec.Image)
	if out, err := dockerRun(context.Background(), args...); err != nil {
		resp.Status = "failed"
		resp.Error = fmt.Sprintf("docker run failed: %v: %s", err, truncate(out, 300))
		return resp
	}
	resp.Status = "completed"
	resp.Success = true
	resp.Result = map[string]interface{}{"container": cname, "image": req.Spec.Image}
	return resp
}

func dockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}

func dockerRun(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
