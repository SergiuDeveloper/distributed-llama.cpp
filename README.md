# distributed-llama.cpp

Runs LLM inference across multiple machines. A central **server** accepts OpenAI-compatible HTTP requests and routes them to **client** nodes, each running llama.cpp in Docker. All communication is secured with mutual TLS using a single shared certificate.

---

## How it works

### Server

The server does two things: it manages clients over gRPC, and it exposes an OpenAI-compatible HTTP API.

When a client connects, the server dials back to it (dial-back pattern), checks whether it already has the model cached, and if not streams the model file to it in 256 KB chunks. It then tells the client to start its llama.cpp Docker container. The connection stays open for the lifetime of the client session. The server uses it to send commands and the client uses it to heartbeat.

When an inference request arrives over HTTP, the server polls all connected clients for their current capacity (how many concurrent in-flight requests they have free), sorts by most available slots and lowest average latency, and forwards the request to the best one. If all clients are at capacity it still routes to one, chosen at random.

### Client

The client connects to the server, registers itself, and waits for commands. It runs a local gRPC server (the **agent**) on a separate port so the server can dial back for capacity queries and inference forwarding.

After receiving the model (or confirming it's already cached), the client pulls the appropriate llama.cpp Docker image and starts a container with `--parallel MAX_SLOTS`, mounting the model file read-only. It polls llama.cpp's `/health` endpoint every 2 seconds until the model is fully loaded, then sends a single hardcoded warmup prompt. Once the warmup completes the client marks itself ready and begins reporting capacity to the server.

Capacity is queried directly from llama.cpp's `/slots` endpoint, which returns one object per slot with a `state` field. The client counts idle slots (state = 0) and returns that number to the server for routing decisions.

The client sends a heartbeat to the server every 10 seconds. After 10 consecutive failures it stops the Docker container and triggers a reconnect. If the server shuts down cleanly (sends `StopContainer` then closes the stream), the client exits instead of reconnecting.

GPU mode is detected automatically via `nvidia-smi`. It can be overridden with the `GPU_MODE` env var.

### Security

All gRPC communication uses mutual TLS. Both server and clients share a single `.pem` file containing a self-signed certificate and private key. Each side verifies the other's certificate against the same CA. The server's dial-back to client agent addresses skips hostname verification (since client IPs are not in the certificate SANs) but still validates the certificate chain.

---

## Project structure

```
src/
  server/       coordinator gRPC service + HTTP API
  client/       inference node agent
  shared/       mTLS, config, model streaming
  proto/        protobuf definitions
generated/      generated gRPC code (do not edit)
tst/
  e2e/          end-to-end tests
bin/
  server/       server binary + .env
  client/       client binary + .env
```

---

## Setup

### 1. Install tools

Install [protoc](https://github.com/protocolbuffers/protobuf/releases), then:

```bash
make install-tools
```

### 2. Generate gRPC code

```bash
make proto
```

This generates Go code from `src/proto/inference.proto` into `generated/inference/`.

### 3. Generate the shared TLS certificate

```bash
make gen-cert SERVER_IP=192.168.1.100
```

Replace `192.168.1.100` with the LAN IP of the machine running the server. This creates `shared.pem`. Copy it to every client machine alongside the client binary.

For local testing only:

```bash
make gen-cert SERVER_IP=127.0.0.1
```

### 4. Configure

Copy the example env files and fill them in:

```bash
cp examples/env/.env.server bin/server/.env
cp examples/env/.env.client bin/client/.env
```

### 5. Build

```bash
make build
```

Produces `bin/server/server(.exe)` and `bin/client/client(.exe)`.

---

## Configuration

### Server

| Variable     | Default        | Description                                          |
| ------------ | -------------- | ---------------------------------------------------- |
| `MODEL_PATH` | -              | Path to the `.gguf` model file to serve to clients   |
| `PEM_PATH`   | `./shared.pem` | Path to the shared mTLS certificate                  |
| `GRPC_PORT`  | `50051`        | Port for the gRPC coordinator (clients connect here) |
| `HTTP_PORT`  | `8181`         | Port for the OpenAI-compatible HTTP API              |

### Client

| Variable            | Default        | Description                                                                    |
| ------------------- | -------------- | ------------------------------------------------------------------------------ |
| `SERVER_ADDR`       | -              | Server address in `host:port` format, e.g. `192.168.1.100:50051`               |
| `PEM_PATH`          | `./shared.pem` | Path to the shared mTLS certificate                                            |
| `MODEL_STORAGE_DIR` | `./models`     | Directory where downloaded model files are stored (created automatically)      |
| `GPU_MODE`          | `auto`         | `auto` detects NVIDIA GPU via `nvidia-smi`, `gpu` forces GPU, `cpu` forces CPU |
| `AGENT_PORT`        | `50052`        | Port the client's gRPC agent listens on (server dials back here)               |
| `LLAMA_PORT`        | `18080`        | Host port mapped to the llama.cpp Docker container                             |
| `MAX_SLOTS`         | `16`           | Maximum number of concurrent inference requests this client will accept        |

---

## Running

Each binary reads `.env` from its working directory.

```bash
cd bin/server && ./server
cd bin/client && ./client
```

Place `shared.pem` at the path specified via the configuration. On client machines, the model is downloaded automatically on first connect.

---

## Testing

The e2e test sends a chat completion request to a running server and verifies the response contains at least one character.

**The server (and at least one client) must already be running before the test is executed.**

```bash
go test ./tst/e2e/ -v -timeout 5m -count=1
```

By default the test hits `http://localhost:8181`. Override with `SERVER_ADDR`:

```bash
SERVER_ADDR=http://192.168.1.100:8181 go test ./tst/e2e/ -v -timeout 5m -count=1
```

The `-timeout 5m` flag is recommended since inference can take time depending on hardware and model size.

---

## Deployment layout (multi-machine)

```
Server machine
├── bin/server/server
├── bin/server/.env        (MODEL_PATH points to your .gguf file)
├── bin/server/shared.pem
└── bin/server/models/
    └── your-model.gguf

Client machine(s)
├── bin/client/client
├── bin/client/.env        (SERVER_ADDR points to server machine)
└── bin/client/shared.pem  (same file as on server)
```

The model is transferred automatically from server to client on first connect. Subsequent restarts skip the transfer if the local file matches the server's file size.

---

## License

MIT - see [LICENSE](LICENSE).
