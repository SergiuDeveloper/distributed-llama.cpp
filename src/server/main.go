package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"

	pb "distributed-llama/generated/inference"
	"distributed-llama/src/shared"
)

func main() {
	cfg, err := shared.LoadServerConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	creds, err := shared.ForServer(cfg.PEMPath)
	if err != nil {
		log.Fatalf("mtls: %v", err)
	}

	reg := newRegistry()

	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		log.Fatalf("listen gRPC :%d: %v", cfg.GRPCPort, err)
	}

	grpcSrv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterCoordinatorServiceServer(grpcSrv, &coordinatorServer{cfg: cfg, reg: reg})

	go func() {
		log.Printf("[server] gRPC listening on :%d", cfg.GRPCPort)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Printf("[server] gRPC error: %v", err)
		}
	}()

	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      newAPIHandler(reg),
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
	}
	go func() {
		log.Printf("[server] HTTP listening on :%d", cfg.HTTPPort)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[server] HTTP error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("[server] received %v - shutting down", sig)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	for _, e := range reg.all() {
		if _, err := e.agentClient.StopContainer(stopCtx, &pb.StopContainerRequest{}); err != nil {
			log.Printf("[server] stop container on %s: %v", e.id[:8], err)
		}
	}

	httpSrv.Shutdown(context.Background())
	grpcSrv.Stop()
	log.Println("[server] shutdown complete")
}

type coordinatorServer struct {
	pb.UnimplementedCoordinatorServiceServer
	cfg *shared.ServerConfig
	reg *clientRegistry
}

func (s *coordinatorServer) Register(req *pb.RegisterRequest, stream pb.CoordinatorService_RegisterServer) error {
	log.Printf("[coordinator] client registered: id=%s host=%s agent=%s", req.ClientId, req.Hostname, req.AgentAddr)

	creds, err := shared.ForClientDialBack(s.cfg.PEMPath)
	if err != nil {
		return fmt.Errorf("mtls for dial-back: %w", err)
	}
	conn, err := grpc.NewClient(req.AgentAddr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial-back to %s: %w", req.AgentAddr, err)
	}

	entry := &clientEntry{
		id:          req.ClientId,
		hostname:    req.Hostname,
		agentAddr:   req.AgentAddr,
		conn:        conn,
		agentClient: pb.NewAgentServiceClient(conn),
		lastSeen:    time.Now(),
	}
	s.reg.add(entry)
	defer func() {
		log.Printf("[coordinator] client disconnected: %s", req.ClientId[:8])
		s.reg.remove(req.ClientId)
	}()

	modelInfo, err := os.Stat(s.cfg.ModelPath)
	if err != nil {
		return fmt.Errorf("stat model: %w", err)
	}
	modelFilename := filepath.Base(s.cfg.ModelPath)

	needsUpload := !req.ModelAlreadyCached ||
		req.CachedModelFilename != modelFilename ||
		req.CachedModelSize != modelInfo.Size()

	if needsUpload {
		log.Printf("[coordinator] uploading model to client %s", req.ClientId[:8])
		if err := stream.Send(&pb.ServerCommand{
			Command: &pb.ServerCommand_BeginUpload{
				BeginUpload: &pb.BeginModelUploadCmd{
					Filename:  modelFilename,
					TotalSize: modelInfo.Size(),
				},
			},
		}); err != nil {
			return fmt.Errorf("send BeginModelUpload: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	} else {
		log.Printf("[coordinator] client %s already has correct model", req.ClientId[:8])
	}

	if err := stream.Send(&pb.ServerCommand{
		Command: &pb.ServerCommand_StartContainer{
			StartContainer: &pb.StartContainerCmd{
				ModelFilename: modelFilename,
				UseGpu:        false,
			},
		},
	}); err != nil {
		return fmt.Errorf("send StartContainerCmd: %w", err)
	}

	<-stream.Context().Done()
	return nil
}

func (s *coordinatorServer) UploadModel(req *pb.UploadModelRequest, stream pb.CoordinatorService_UploadModelServer) error {
	log.Printf("[coordinator] UploadModel %q", req.Filename)

	if filepath.Base(s.cfg.ModelPath) != req.Filename {
		return fmt.Errorf("unknown model file %q (server has %q)", req.Filename, filepath.Base(s.cfg.ModelPath))
	}

	f, err := os.Open(s.cfg.ModelPath)
	if err != nil {
		return fmt.Errorf("open model: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat model: %w", err)
	}

	log.Printf("[coordinator] streaming %s (%d bytes)", req.Filename, info.Size())
	if err := shared.StreamChunks(f, info.Size(), stream.Send); err != nil {
		return fmt.Errorf("stream chunks: %w", err)
	}
	log.Printf("[coordinator] upload complete: %s", req.Filename)
	return nil
}

func (s *coordinatorServer) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	s.reg.touch(req.ClientId)
	return &pb.HeartbeatResponse{Ok: true}, nil
}
