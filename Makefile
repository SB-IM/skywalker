# SSH private key set up.
CURRENT_USER ?= william
PRIVATE_KEY_FILE ?= id_ed25519
PRIVATE_KEY_PATH ?= github=$(shell getent passwd "$(CURRENT_USER)" | cut -d: -f6)/.ssh/$(PRIVATE_KEY_FILE)

# Enable docker buildkit.
DOCKER_BUILDKIT = 1
# Project image repo.
IMAGE ?= ghcr.io/sb-im/skywalker
IMAGE_TAG ?= latest
# Docker-compose file.
DOCKER_COMPOSE_FILE ?= docker/docker-compose.yml
# Docker-compose service.
SERVICE ?=

# Version info for binaries
GIT_REVISION := $(shell git rev-parse --short HEAD)
GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD)
GIT_TAG := $(shell git describe --tags)

# go build flags.
VPREFIX := github.com/SB-IM/skywalker/cmd/build
GO_LDFLAGS := -X $(VPREFIX).Branch=$(GIT_BRANCH) -X $(VPREFIX).Version=$(GIT_TAG) -X $(VPREFIX).Revision=$(GIT_REVISION) -X $(VPREFIX).BuildUser=$(shell whoami)@$(shell hostname) -X $(VPREFIX).BuildDate=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GO_FLAGS := -ldflags "-extldflags \"-static\" -s -w $(GO_LDFLAGS)" -a -installsuffix cgo
# See: https://golang.org/doc/gdb#Introduction
DEBUG_GO_FLAGS := -race -gcflags "all=-N -l" -ldflags "-extldflags \"-static\" $(GO_LDFLAGS)"
# go build with -race flag must enable cgo.
CGO_ENABLED := 0

DEBUG ?= false
ifeq ($(DEBUG), true)
	IMAGE_TAG := debug
	GO_FLAGS := $(DEBUG_GO_FLAGS)
	CGO_ENABLED := 1
endif

.PHONY: run
run:
	@DEBUG_MQTT_CLIENT=false go run -race ./cmd --debug $(SERVICE) -c config/config.dev.toml

skywalker:
	CGO_ENABLED=$(CGO_ENABLED) GOOS=linux go build $(GO_FLAGS) -o $@ ./cmd

.PHONY: lint
lint:
	@golangci-lint run ./...

.PHONY: image
image:
	@docker build \
	--build-arg DEBUG=$(DEBUG) \
	--ssh $(PRIVATE_KEY_PATH) \
	-t $(IMAGE):$(IMAGE_TAG) \
	-f docker/Dockerfile \
	.

.PHONY: push
push:
	@docker push $(IMAGE)

# Note: '--env-file' value is relative to '-f' value's directory.
.PHONY: up
up: down image
	@docker-compose -f $(DOCKER_COMPOSE_FILE) up -d

.PHONY: down
down:
	@docker-compose -f $(DOCKER_COMPOSE_FILE) down --remove-orphans

.PHONY: logs
logs:
	@docker-compose -f $(DOCKER_COMPOSE_FILE) logs -f $(SERVICE)

.PHONY: run-mosquitto
run-mosquitto:
	@docker run -d --rm --name mosquitto -p 1883:1883 -p 9001:9001 -v $$PWD/config/mosquitto.conf:/mosquitto/config/mosquitto.conf eclipse-mosquitto:2

.PHONY: stop-mosquitto
stop-mosquitto:
	@docker stop mosquitto

.PHONY: run-turn
run-turn:
	@docker run -it --rm --name turn --network host -v $$PWD/config/config.docker.toml:/etc/skywalker/config.toml:ro ghcr.io/sb-im/skywalker:debug --debug turn -c /etc/skywalker/config.toml

.PHONY: stop-turn
stop-turn:
	@docker stop turn

.PHONY: e2e-broadcast
e2e-broadcast:
	@go run ./e2e/broadcast

.PHONY: clean
clean:
	@rm -rf skywalker
