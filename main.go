// SPDX-License-Identifier: AGPL-3.0-or-later

// comfyui-shim exposes an OpenAI-compatible image surface (/v1/models,
// /v1/images/generations) in front of ComfyUI's native workflow API, so ComfyUI
// can be registered as a custom OpenAI provider in the bifrost gateway.
//
// Each entry in models.json is one OpenAI "model" bound to a ComfyUI workflow
// template (API/prompt format) under workflows/; per-model bindings say which node
// input carries the prompt / negative / seed / size / checkpoint, so one generic
// submit path drives every workflow. Config (models.json + workflows/*.json) is read
// at runtime so workflows are updatable through the pipeline without rebuilding.
//
// stdlib only -> a single static binary. Env:
//
//	COMFYUI_URL  upstream ComfyUI          (default http://127.0.0.1:8188)
//	SHIM_ADDR    listen address           (default 0.0.0.0:8189)
//	SHIM_CONFIG  path to models.json       (default ./config/models.json)
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	pollInterval = 1 * time.Second
	genTimeout   = 10 * time.Minute // an SDXL/Flux batch can take minutes; never abandon mid-run
	httpTimeout  = 60 * time.Second
)

type modelCfg struct {
	Workflow   string              `json:"workflow"`
	Checkpoint string              `json:"checkpoint"`
	Bindings   map[string][]string `json:"bindings"`
	Defaults   map[string]any      `json:"defaults"`
	graph      map[string]any      // loaded workflow template (deep-copied per request)
}

type config struct {
	DefaultModel string              `json:"default_model"`
	Models       map[string]modelCfg `json:"models"`
}

var (
	comfyURL = envOr("COMFYUI_URL", "http://127.0.0.1:8188")
	addr     = envOr("SHIM_ADDR", "0.0.0.0:8189")
	cfgPath  = envOr("SHIM_CONFIG", "./config/models.json")
	cfg      config
	client   = &http.Client{Timeout: httpTimeout}
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadConfig() error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	wfDir := filepath.Join(filepath.Dir(cfgPath), "workflows")
	for name, m := range cfg.Models {
		wfRaw, err := os.ReadFile(filepath.Join(wfDir, m.Workflow))
		if err != nil {
			return fmt.Errorf("model %q workflow: %w", name, err)
		}
		if err := json.Unmarshal(wfRaw, &m.graph); err != nil {
			return fmt.Errorf("model %q workflow parse: %w", name, err)
		}
		cfg.Models[name] = m
	}
	return nil
}

// comfyDo issues a JSON request to ComfyUI and decodes the response into out.
func comfyDo(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, comfyURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("comfyui %s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func comfyView(filename, subfolder, ftype string) ([]byte, error) {
	q := url.Values{"filename": {filename}, "subfolder": {subfolder}, "type": {ftype}}
	resp, err := client.Get(comfyURL + "/view?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("comfyui /view -> %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// deepCopy round-trips a value through JSON to isolate it per request.
func deepCopy(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// setBinding sets graph[nodeID].inputs[field] = value.
func setBinding(graph map[string]any, binding []string, value any) {
	if len(binding) != 2 {
		return
	}
	node, ok := graph[binding[0]].(map[string]any)
	if !ok {
		return
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		return
	}
	inputs[binding[1]] = value
}

func parseSize(size string) (int, int) {
	if size == "" || size == "auto" {
		return 1024, 1024
	}
	parts := strings.SplitN(strings.ToLower(size), "x", 2)
	if len(parts) == 2 {
		w, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if e1 == nil && e2 == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return 1024, 1024
}

func randSeed() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<48))
	if err != nil {
		return time.Now().UnixNano() & ((1 << 48) - 1)
	}
	return n.Int64()
}

func buildGraph(m modelCfg, prompt, negative string, width, height int, seed int64) map[string]any {
	g := deepCopy(m.graph)
	b := m.Bindings
	if bd, ok := b["positive"]; ok {
		setBinding(g, bd, prompt)
	}
	if bd, ok := b["negative"]; ok {
		neg := negative
		if neg == "" {
			if dn, ok := m.Defaults["negative"].(string); ok {
				neg = dn
			}
		}
		setBinding(g, bd, neg)
	}
	if bd, ok := b["checkpoint"]; ok && m.Checkpoint != "" {
		setBinding(g, bd, m.Checkpoint)
	}
	if bd, ok := b["seed"]; ok {
		setBinding(g, bd, seed)
	}
	if bd, ok := b["width"]; ok {
		setBinding(g, bd, width)
	}
	if bd, ok := b["height"]; ok {
		setBinding(g, bd, height)
	}
	for _, key := range []string{"steps", "cfg", "sampler", "scheduler", "denoise"} {
		if bd, ok := b[key]; ok {
			if dv, ok := m.Defaults[key]; ok {
				setBinding(g, bd, dv)
			}
		}
	}
	return g
}

func generate(model, prompt, negative string, n, width, height int, seed *int64) ([][]byte, error) {
	m, ok := cfg.Models[model]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", model)
	}
	var images [][]byte
	for i := 0; i < max(1, n); i++ {
		s := randSeed()
		if seed != nil {
			s = (*seed + int64(i)) & ((1 << 48) - 1)
		}
		g := buildGraph(m, prompt, negative, width, height, s)
		var sub struct {
			PromptID string `json:"prompt_id"`
		}
		if err := comfyDo("POST", "/prompt", map[string]any{"prompt": g}, &sub); err != nil {
			return nil, err
		}
		outputs, err := waitForOutputs(sub.PromptID)
		if err != nil {
			return nil, err
		}
		for _, node := range outputs {
			nm, _ := node.(map[string]any)
			imgs, _ := nm["images"].([]any)
			for _, iv := range imgs {
				im, _ := iv.(map[string]any)
				if t, _ := im["type"].(string); t == "temp" {
					continue
				}
				fn, _ := im["filename"].(string)
				sf, _ := im["subfolder"].(string)
				tp, _ := im["type"].(string)
				if tp == "" {
					tp = "output"
				}
				data, err := comfyView(fn, sf, tp)
				if err != nil {
					return nil, err
				}
				images = append(images, data)
			}
		}
	}
	return images, nil
}

func waitForOutputs(promptID string) (map[string]any, error) {
	deadline := time.Now().Add(genTimeout)
	for time.Now().Before(deadline) {
		var hist map[string]struct {
			Outputs map[string]any `json:"outputs"`
		}
		if err := comfyDo("GET", "/history/"+promptID, nil, &hist); err != nil {
			return nil, err
		}
		if h, ok := hist[promptID]; ok && len(h.Outputs) > 0 {
			return h.Outputs, nil
		}
		time.Sleep(pollInterval)
	}
	return nil, fmt.Errorf("comfyui did not finish prompt %s within %s", promptID, genTimeout)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error"}})
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(cfg.Models))
	for name := range cfg.Models {
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "comfyui"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type genRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Negative       string `json:"negative_prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Seed           *int64 `json:"seed"`
	ResponseFormat string `json:"response_format"`
}

func handleGenerations(w http.ResponseWriter, r *http.Request) {
	var req genRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Model == "" {
		req.Model = cfg.DefaultModel
	}
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "'prompt' is required")
		return
	}
	if _, ok := cfg.Models[req.Model]; !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q; see GET /v1/models", req.Model))
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	width, height := parseSize(req.Size)
	imgs, err := generate(req.Model, req.Prompt, req.Negative, req.N, width, height, req.Seed)
	if err != nil {
		log.Printf("generation failed: %v", err)
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("generation failed: %v", err))
		return
	}
	data := make([]map[string]any, 0, len(imgs))
	for _, img := range imgs {
		data = append(data, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(img)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("config: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "comfyui": comfyURL})
	})
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("POST /v1/images/generations", handleGenerations)

	names := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		names = append(names, n)
	}
	log.Printf("comfyui-shim on %s -> %s (%d models: %s)", addr, comfyURL, len(cfg.Models), strings.Join(names, ", "))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
