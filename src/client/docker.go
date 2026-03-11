package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	llamaCPUImage = "ghcr.io/ggml-org/llama.cpp:server"
	llamaGPUImage = "ghcr.io/ggml-org/llama.cpp:server-cuda"
	containerName = "llama-agent"
	containerPort = "8080/tcp"
	warmupPrompt  = "Hello"
)

type Manager struct {
	cli         *client.Client
	containerID string
	llamaPort   string
	maxSlots    int
	ready       atomic.Bool
}

func newDockerManager(llamaPort, maxSlots int) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Manager{cli: cli, llamaPort: strconv.Itoa(llamaPort), maxSlots: maxSlots}, nil
}

func (m *Manager) Start(ctx context.Context, modelPath string, useGPU bool) error {
	if m.containerID != "" {
		return fmt.Errorf("container already running (id=%s); call Stop first", m.containerID)
	}

	imgName := llamaCPUImage
	if useGPU {
		imgName = llamaGPUImage
	}

	log.Printf("[docker] pulling %s", imgName)
	reader, err := m.cli.ImagePull(ctx, imgName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull %s: %w", imgName, err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	absModel, err := filepath.Abs(modelPath)
	if err != nil {
		return fmt.Errorf("resolve model path: %w", err)
	}
	modelFilename := filepath.Base(absModel)
	containerModelPath := "/models/" + modelFilename

	containerCfg := &container.Config{
		Image: imgName,
		Cmd: []string{
			"--model", containerModelPath,
			"--port", "8080",
			"--host", "0.0.0.0",
			"--parallel", strconv.Itoa(m.maxSlots),
		},
		ExposedPorts: nat.PortSet{
			nat.Port(containerPort): struct{}{},
		},
	}

	hostCfg := &container.HostConfig{
		Binds: []string{absModel + ":" + containerModelPath + ":ro"},
		PortBindings: nat.PortMap{
			nat.Port(containerPort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: m.llamaPort}},
		},
	}

	if useGPU {
		hostCfg.DeviceRequests = []container.DeviceRequest{
			{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}},
		}
	}

	_ = m.cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	resp, err := m.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	m.containerID = resp.ID

	if err := m.cli.ContainerStart(ctx, m.containerID, container.StartOptions{}); err != nil {
		m.containerID = ""
		return fmt.Errorf("container start: %w", err)
	}

	log.Printf("[docker] started (id=%s, gpu=%v, model=%s, slots=%d) - waiting for ready", m.containerID[:12], useGPU, modelFilename, m.maxSlots)
	m.ready.Store(false)
	go m.waitReady()
	return nil
}

func (m *Manager) waitReady() {
	url := "http://127.0.0.1:" + m.llamaPort + "/health"
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	log.Printf("[docker] llama.cpp ready — sending warmup prompt")
	if err := m.warmup(); err != nil {
		log.Printf("[docker] warmup failed: %v", err)
	} else {
		log.Printf("[docker] warmup done")
	}

	m.ready.Store(true)
	log.Printf("[docker] client ready on :%s (%d slots)", m.llamaPort, m.maxSlots)
}

func (m *Manager) warmup() error {
	url := "http://127.0.0.1:" + m.llamaPort + "/v1/chat/completions"
	body, _ := json.Marshal(map[string]any{
		"model":      "local",
		"messages":   []map[string]string{{"role": "user", "content": warmupPrompt}},
		"max_tokens": 16,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (m *Manager) QuerySlots() (int32, error) {
	resp, err := http.Get("http://127.0.0.1:" + m.llamaPort + "/slots")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var slots []struct {
		State int `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return 0, err
	}
	var idle int32
	for _, s := range slots {
		if s.State == 0 {
			idle++
		}
	}
	return idle, nil
}

func (m *Manager) Stop(ctx context.Context) error {
	if m.containerID == "" {
		return nil
	}
	id := m.containerID
	timeout := 30
	stopCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	log.Printf("[docker] stopping %s", id[:12])
	if err := m.cli.ContainerStop(stopCtx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		log.Printf("[docker] stop warning: %v", err)
	}
	_ = m.cli.ContainerRemove(stopCtx, id, container.RemoveOptions{Force: true})
	m.containerID = ""
	m.ready.Store(false)
	log.Println("[docker] container stopped")
	return nil
}

func (m *Manager) IsRunning() bool {
	return m.containerID != "" && m.ready.Load()
}

func (m *Manager) Close() error {
	return m.cli.Close()
}

func (m *Manager) LogsToStdout(ctx context.Context) {
	if m.containerID == "" {
		return
	}
	go func() {
		out, err := m.cli.ContainerLogs(ctx, m.containerID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
		})
		if err != nil {
			return
		}
		defer out.Close()
		_, _ = io.Copy(os.Stdout, out)
	}()
}
