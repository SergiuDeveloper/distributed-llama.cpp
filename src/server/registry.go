package main

import (
	"sync"
	"time"

	pb "distributed-llama/generated/inference"

	"google.golang.org/grpc"
)

const latencyAlpha = 0.2

type clientEntry struct {
	id          string
	hostname    string
	agentAddr   string
	conn        *grpc.ClientConn
	agentClient pb.AgentServiceClient
	lastSeen    time.Time

	mu           sync.Mutex
	avgLatencyMs float64
}

func (e *clientEntry) updateLatency(ms int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.avgLatencyMs == 0 {
		e.avgLatencyMs = float64(ms)
	} else {
		e.avgLatencyMs = latencyAlpha*float64(ms) + (1-latencyAlpha)*e.avgLatencyMs
	}
}

func (e *clientEntry) getAvgLatency() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.avgLatencyMs
}

type clientRegistry struct {
	mu      sync.RWMutex
	clients map[string]*clientEntry
}

func newRegistry() *clientRegistry {
	return &clientRegistry{clients: make(map[string]*clientEntry)}
}

func (r *clientRegistry) add(e *clientEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.clients[e.id]; ok {
		old.conn.Close()
	}
	r.clients[e.id] = e
}

func (r *clientRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.clients[id]; ok {
		e.conn.Close()
		delete(r.clients, id)
	}
}

func (r *clientRegistry) all() []*clientEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*clientEntry, 0, len(r.clients))
	for _, e := range r.clients {
		out = append(out, e)
	}
	return out
}

func (r *clientRegistry) touch(id string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.clients[id]; ok {
		e.lastSeen = time.Now()
	}
}

func (r *clientRegistry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}
