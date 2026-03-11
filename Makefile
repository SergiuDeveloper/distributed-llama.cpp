.PHONY: proto install-tools build-server build-client build gen-cert clean

PROTO_DIR   = src/proto
PROTO_FILE  = $(PROTO_DIR)/inference.proto

ifeq ($(OS),Windows_NT)
	EXT = .exe
else
	EXT =
endif

SERVER_BIN  = bin/server/server$(EXT)
CLIENT_BIN  = bin/client/client$(EXT)

proto: $(PROTO_FILE)
	protoc \
		--go_out=. \
		--go_opt=module=distributed-llama \
		--go-grpc_out=. \
		--go-grpc_opt=module=distributed-llama \
		$(PROTO_FILE)

install-tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

build: build-server build-client

build-server:
	mkdir -p bin/server || true
	go build -o $(SERVER_BIN) ./src/server

build-client:
	mkdir -p bin/client || true
	go build -o $(CLIENT_BIN) ./src/client

SERVER_IP ?= 127.0.0.1

gen-cert:
	printf '[req]\ndistinguished_name=dn\n[dn]\n' > _openssl.cnf
	OPENSSL_CONF=_openssl.cnf MSYS_NO_PATHCONV=1 openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
		-keyout _key.pem -out _cert.pem -days 3650 -nodes \
		-subj "/CN=distributed-llama" \
		-addext "subjectAltName=IP:$(SERVER_IP),IP:127.0.0.1,DNS:localhost"
	cat _cert.pem _key.pem > shared.pem
	rm -f _openssl.cnf _cert.pem _key.pem
	@echo "shared.pem generated. Copy it to all client machines."

clean:
	rm -rf bin/ generated/inference/
