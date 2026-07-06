NAME=owrt-lite
BIN_DIR=bin
PKG=owrt-lite/cmd/owrt-lite
LDFLAGS=-s -w
BUILD_FLAGS=-trimpath -ldflags "$(LDFLAGS)"

.PHONY: all clean \
	build-linux-amd64 build-linux-arm build-linux-arm64 build-linux-mipsle-soft build-linux-mips-soft \
	build-windows-amd64 build-windows-arm64 \
	build-darwin-amd64 build-darwin-arm64 \
	build-freebsd-amd64

all: build-linux-amd64 build-linux-arm build-linux-arm64 build-linux-mipsle-soft build-linux-mips-soft \
	build-windows-amd64 build-windows-arm64 build-darwin-amd64 build-darwin-arm64 build-freebsd-amd64

clean:
	@rm -rf $(BIN_DIR)

build-linux-amd64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-linux-amd64 ./cmd/owrt-lite

build-linux-arm:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-linux-armv7 ./cmd/owrt-lite

build-linux-arm64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-linux-arm64 ./cmd/owrt-lite

build-linux-mipsle-soft:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-linux-mipsle-softfloat ./cmd/owrt-lite

build-linux-mips-soft:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=mips GOMIPS=softfloat go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-linux-mips-softfloat ./cmd/owrt-lite

build-windows-amd64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-windows-amd64.exe ./cmd/owrt-lite

build-windows-arm64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-windows-arm64.exe ./cmd/owrt-lite

build-darwin-amd64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-darwin-amd64 ./cmd/owrt-lite

build-darwin-arm64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-darwin-arm64 ./cmd/owrt-lite

build-freebsd-amd64:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(NAME)-freebsd-amd64 ./cmd/owrt-lite
