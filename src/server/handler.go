package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"time"

	pb "distributed-llama/generated/inference"
)

type apiHandler struct {
	reg *clientRegistry
	mux *http.ServeMux
}

func newAPIHandler(reg *clientRegistry) *apiHandler {
	h := &apiHandler{reg: reg, mux: http.NewServeMux()}
	h.mux.HandleFunc("/v1/chat/completions", h.handleInference)
	h.mux.HandleFunc("/v1/completions", h.handleInference)
	h.mux.HandleFunc("/v1/embeddings", h.handleInference)
	h.mux.HandleFunc("/health", h.handleHealth)
	return h
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *apiHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","clients":%d}`, h.reg.count())
}

func (h *apiHandler) handleInference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	clients := h.reg.all()
	if len(clients) == 0 {
		writeError(w, http.StatusServiceUnavailable, "no inference clients connected")
		return
	}

	target := routeRequest(pollCapacity(r.Context(), clients))
	if target == nil {
		target = clients[rand.Intn(len(clients))]
		log.Printf("[api] no client reported available slots - randomly assigned %s", target.id[:8])
	}

	hdrs := make(map[string]string, len(r.Header))
	for k := range r.Header {
		hdrs[k] = r.Header.Get(k)
	}

	reqID := fmt.Sprintf("%x", time.Now().UnixNano())
	start := time.Now()

	inferCtx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	resp, err := target.agentClient.ForwardInference(inferCtx, &pb.InferenceRequest{
		RequestId: reqID,
		BodyJson:  body,
		Headers:   hdrs,
		Path:      r.URL.Path,
		Method:    r.Method,
	})
	if err != nil {
		log.Printf("[api] forward to %s failed: %v", target.id[:8], err)
		writeError(w, http.StatusBadGateway, "inference client error: "+err.Error())
		return
	}

	latency := time.Since(start)
	target.updateLatency(latency.Milliseconds())
	log.Printf("[api] %s → client %s in %dms (status=%d)",
		r.URL.Path, target.id[:8], latency.Milliseconds(), resp.StatusCode)

	for k, v := range resp.Headers {
		switch k {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(int(resp.StatusCode))
	w.Write(resp.BodyJson)
}

type errResp struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errResp{Error: msg})
}
