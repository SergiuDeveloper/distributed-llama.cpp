package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	pb "distributed-llama/generated/inference"
	"distributed-llama/src/shared"
)

func main() {
	cfg, err := shared.LoadClientConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	useGPU := resolveGPUMode(cfg.GPUMode)
	log.Printf("[client] compute mode: %s", gpuLabel(useGPU))

	clientID := newID()
	log.Printf("[client] id=%s", clientID)

	agentSrv, agentAddr, dockerMgr := startAgentServer(cfg, useGPU, clientID)
	defer func() {
		agentSrv.GracefulStop()
		dockerMgr.Stop(context.Background())
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[client] received %v - shutting down", sig)
		dockerMgr.Stop(context.Background())
		agentSrv.GracefulStop()
		os.Exit(0)
	}()

	for {
		err := runSession(cfg, clientID, agentAddr, useGPU, dockerMgr)
		if err == errShutdown {
			log.Printf("[client] server shut down - exiting")
			return
		}
		if err != nil {
			log.Printf("[client] session ended: %v", err)
		}
		log.Printf("[client] reconnecting in 10s")
		time.Sleep(10 * time.Second)
	}
}

var errShutdown = fmt.Errorf("server shutdown")

func runSession(cfg *shared.ClientConfig, clientID, agentAddr string, useGPU bool, dockerMgr *Manager) error {
	creds, err := shared.ForClient(cfg.PEMPath, serverHost(cfg.ServerAddr))
	if err != nil {
		return fmt.Errorf("mtls: %w", err)
	}

	conn, err := grpc.NewClient(cfg.ServerAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.ServerAddr, err)
	}
	defer conn.Close()

	coordClient := pb.NewCoordinatorServiceClient(conn)
	modelCached, cachedFilename, cachedSize := checkLocalModel(cfg.ModelStorageDir)

	stream, err := coordClient.Register(context.Background(), &pb.RegisterRequest{
		ClientId:            clientID,
		Hostname:            hostname(),
		AgentAddr:           agentAddr,
		ModelAlreadyCached:  modelCached,
		CachedModelFilename: cachedFilename,
		CachedModelSize:     cachedSize,
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	log.Printf("[client] registered with %s", cfg.ServerAddr)

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()
	go runHeartbeat(sessionCtx, coordClient, clientID, dockerMgr)

	var modelPath string
	if cached, filename, _ := checkLocalModel(cfg.ModelStorageDir); cached {
		modelPath, _ = filepath.Abs(filepath.Join(cfg.ModelStorageDir, filename))
	}
	var stopped bool
	for {
		cmd, err := stream.Recv()
		if err == io.EOF {
			if stopped {
				return errShutdown
			}
			return fmt.Errorf("server closed the stream")
		}
		if err != nil {
			return fmt.Errorf("stream recv: %w", err)
		}

		switch c := cmd.Command.(type) {
		case *pb.ServerCommand_BeginUpload:
			log.Printf("[client] downloading model %s (%d bytes)", c.BeginUpload.Filename, c.BeginUpload.TotalSize)
			absPath, err := downloadModel(sessionCtx, coordClient, cfg.ModelStorageDir, c.BeginUpload.Filename, c.BeginUpload.TotalSize)
			if err != nil {
				log.Printf("[client] download failed: %v", err)
			} else {
				modelPath = absPath
				log.Printf("[client] model at %s", modelPath)
			}

		case *pb.ServerCommand_StartContainer:
			log.Printf("[client] starting container (model=%s)", c.StartContainer.ModelFilename)
			if modelPath == "" {
				log.Printf("[client] no model yet - skipping start")
				continue
			}
			if err := dockerMgr.Start(context.Background(), modelPath, useGPU); err != nil {
				log.Printf("[client] docker start: %v", err)
			}

		case *pb.ServerCommand_StopContainer:
			log.Printf("[client] stopping container")
			stopped = true
			if err := dockerMgr.Stop(context.Background()); err != nil {
				log.Printf("[client] docker stop: %v", err)
			}
		}
	}
}

func runHeartbeat(ctx context.Context, client pb.CoordinatorServiceClient, clientID string, dockerMgr *Manager) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	const maxFails = 10
	var fails int32

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := client.Heartbeat(hbCtx, &pb.HeartbeatRequest{
				ClientId:  clientID,
				Timestamp: time.Now().UnixMilli(),
			})
			cancel()

			if err != nil {
				n := atomic.AddInt32(&fails, 1)
				log.Printf("[heartbeat] failed (%d/%d): %v", n, maxFails, err)
				if n >= maxFails {
					log.Println("[heartbeat] max failures - stopping container")
					dockerMgr.Stop(context.Background())
					return
				}
			} else {
				atomic.StoreInt32(&fails, 0)
			}
		}
	}
}

func startAgentServer(cfg *shared.ClientConfig, useGPU bool, clientID string) (*grpc.Server, string, *Manager) {
	creds, err := shared.ForServer(cfg.PEMPath)
	if err != nil {
		log.Fatalf("agent mtls: %v", err)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.AgentPort))
	if err != nil {
		log.Fatalf("agent listen :%d: %v", cfg.AgentPort, err)
	}

	dockerMgr, err := newDockerManager(cfg.LlamaPort, cfg.MaxSlots)
	if err != nil {
		log.Fatalf("docker manager: %v", err)
	}

	grpcSrv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterAgentServiceServer(grpcSrv, &agentServer{
		clientID:  clientID,
		useGPU:    useGPU,
		dockerMgr: dockerMgr,
		llamaPort: strconv.Itoa(cfg.LlamaPort),
	})

	go func() {
		log.Printf("[agent] listening on :%d", cfg.AgentPort)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Printf("[agent] error: %v", err)
		}
	}()

	agentAddr := fmt.Sprintf("%s:%d", localIP(), cfg.AgentPort)
	log.Printf("[agent] reachable at %s", agentAddr)
	return grpcSrv, agentAddr, dockerMgr
}

type agentServer struct {
	pb.UnimplementedAgentServiceServer
	clientID  string
	useGPU    bool
	dockerMgr *Manager
	llamaPort string
}

func (a *agentServer) QueryCapacity(ctx context.Context, _ *pb.CapacityRequest) (*pb.CapacityResponse, error) {
	if !a.dockerMgr.IsRunning() {
		log.Printf("[agent] QueryCapacity: container not running")
		return &pb.CapacityResponse{ClientId: a.clientID, EstimatedSlots: -1}, nil
	}

	slots, err := a.dockerMgr.QuerySlots()
	if err != nil {
		log.Printf("[agent] QueryCapacity: slot query failed: %v", err)
		return &pb.CapacityResponse{ClientId: a.clientID, EstimatedSlots: 0}, nil
	}

	return &pb.CapacityResponse{
		ClientId:       a.clientID,
		EstimatedSlots: slots,
	}, nil
}

func (a *agentServer) ForwardInference(ctx context.Context, req *pb.InferenceRequest) (*pb.InferenceResponse, error) {
	start := time.Now()

	path := req.Path
	if path == "" {
		path = "/v1/chat/completions"
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+a.llamaPort+path, bytes.NewReader(req.BodyJson))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("forward to llama.cpp: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	latency := time.Since(start).Milliseconds()
	log.Printf("[agent] served %s %s → status=%d in %dms", method, path, resp.StatusCode, latency)

	respHeaders := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	return &pb.InferenceResponse{
		RequestId:  req.RequestId,
		StatusCode: int32(resp.StatusCode),
		BodyJson:   body,
		Headers:    respHeaders,
		LatencyMs:  latency,
	}, nil
}

func (a *agentServer) StopContainer(ctx context.Context, _ *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	if err := a.dockerMgr.Stop(ctx); err != nil {
		return &pb.StopContainerResponse{Success: false, Message: err.Error()}, nil
	}
	return &pb.StopContainerResponse{Success: true}, nil
}

func resolveGPUMode(cfgMode string) bool {
	if cfgMode == "gpu" {
		return true
	}
	if cfgMode == "cpu" {
		return false
	}
	return exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Run() == nil
}

func gpuLabel(useGPU bool) string {
	if useGPU {
		return "GPU"
	}
	return "CPU"
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func serverHost(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func checkLocalModel(dir string) (cached bool, filename string, size int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "", 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".gguf") {
			info, err := e.Info()
			if err != nil {
				continue
			}
			return true, e.Name(), info.Size()
		}
	}
	return false, "", 0
}

func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
