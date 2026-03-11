package shared

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type ServerConfig struct {
	ModelPath string
	PEMPath   string
	GRPCPort  int
	HTTPPort  int
}

type ClientConfig struct {
	ServerAddr      string
	PEMPath         string
	ModelStorageDir string
	GPUMode         string
	AgentPort       int
	LlamaPort       int
	MaxSlots        int
}

func LoadServerConfig() (*ServerConfig, error) {
	_ = godotenv.Load(".env")

	cfg := &ServerConfig{
		ModelPath: os.Getenv("MODEL_PATH"),
		PEMPath:   envDefault("PEM_PATH", "./shared.pem"),
		GRPCPort:  envInt("GRPC_PORT", 50051),
		HTTPPort:  envInt("HTTP_PORT", 8181),
	}

	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("MODEL_PATH is required")
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return nil, fmt.Errorf("MODEL_PATH %q: %w", cfg.ModelPath, err)
	}
	if _, err := os.Stat(cfg.PEMPath); err != nil {
		return nil, fmt.Errorf("PEM_PATH %q: %w", cfg.PEMPath, err)
	}
	return cfg, nil
}

func LoadClientConfig() (*ClientConfig, error) {
	_ = godotenv.Load(".env")

	cfg := &ClientConfig{
		ServerAddr:      os.Getenv("SERVER_ADDR"),
		PEMPath:         envDefault("PEM_PATH", "./shared.pem"),
		ModelStorageDir: envDefault("MODEL_STORAGE_DIR", "./models"),
		GPUMode:         envDefault("GPU_MODE", "auto"),
		AgentPort:       envInt("AGENT_PORT", 50052),
		LlamaPort:       envInt("LLAMA_PORT", 18080),
		MaxSlots:        envInt("MAX_SLOTS", 16),
	}

	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("SERVER_ADDR is required (e.g. 192.168.1.100:50051)")
	}
	if _, err := os.Stat(cfg.PEMPath); err != nil {
		return nil, fmt.Errorf("PEM_PATH %q: %w", cfg.PEMPath, err)
	}
	if err := os.MkdirAll(cfg.ModelStorageDir, 0o755); err != nil {
		return nil, fmt.Errorf("MODEL_STORAGE_DIR %q: %w", cfg.ModelStorageDir, err)
	}
	switch cfg.GPUMode {
	case "auto", "gpu", "cpu":
	default:
		return nil, fmt.Errorf("GPU_MODE must be auto|gpu|cpu (got %q)", cfg.GPUMode)
	}
	return cfg, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
