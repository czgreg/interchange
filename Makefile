# Makefile — leap-gateway dev / deploy targets.
#
# Quick reference:
#   make help              # list targets
#   make setup-ssh         # 一次性：推公钥 + NOPASSWD sudo（先跑这个！）
#   make test              # go vet + go test
#   make deploy-89         # deploy to staging 89
#   make deploy-92         # deploy to production 92
#   make deploy-all        # 89 先，92 后
#   make deploy-yaml       # 仅推 gateway.yaml（不换二进制）
#   make deploy-gvisor     # 推 gvisor 配置到 89（per_terminal 前置步骤）
#   make deploy-perterm    # 推 per_terminal:true 配置到 89
#   make status            # /api/status
#   make health            # /api/proxies/active
#   make nodes             # /api/nodes/health
#   make logs              # tail leap-gateway 日志
#   make ssh               # ssh 进节点
#
# 首次使用：
#   make setup-ssh NODE=dianwei@192.168.70.89
#   make setup-ssh NODE=dianwei@192.168.70.92

NODE        ?= dianwei@192.168.70.89
NODE_89     := dianwei@192.168.70.89
NODE_92     := dianwei@192.168.70.92
NODE_HOST   := $(word 2,$(subst @, ,$(NODE)))
LEAP_API    ?= http://$(NODE_HOST):18080
GATEWAY_YAML?= ./gateway.yaml
GOOS        ?= linux
GOARCH      ?= amd64
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || git rev-parse --short HEAD)
LDFLAGS     := -s -w -X main.Version=$(VERSION)

SSH_OPTS    := -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o BatchMode=yes
SSH         := ssh $(SSH_OPTS) $(NODE)
SCP         := scp $(SSH_OPTS)

.DEFAULT_GOAL := help

.PHONY: help
help:  ## 列出所有 target
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build:  ## 本地构建 (macOS dev)
	go build -o build/leap-gateway ./cmd/gateway

.PHONY: build-linux
build-linux:  ## 交叉编译 linux/amd64（部署用）
	@mkdir -p build
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build \
	  -ldflags='$(LDFLAGS)' \
	  -o build/leap-gateway-linux-amd64 ./cmd/gateway
	@echo "  built build/leap-gateway-linux-amd64  version=$(VERSION)"

.PHONY: test
test:  ## go vet + go test ./...
	go vet ./...
	go test ./...

.PHONY: stage
stage:  ## 打全量部署 tarball (build/leap-stage.tgz)
	./scripts/stage.sh

# ---------------------------------------------------------------------------
# Deploy
# ---------------------------------------------------------------------------

.PHONY: deploy-fast
deploy-fast: build-linux  ## ⚡ 热替换二进制 + 重启 (~10s)  NODE=dianwei@<ip>
	scripts/deploy.sh --skip-build $(NODE_HOST)

.PHONY: deploy-89
deploy-89: build-linux  ## deploy to staging 89
	scripts/deploy.sh --skip-build 192.168.70.89

.PHONY: deploy-92
deploy-92: build-linux  ## deploy to production 92
	scripts/deploy.sh --skip-build 192.168.70.92

.PHONY: deploy-all
deploy-all: build-linux  ## deploy to 89 then 92 in sequence
	scripts/deploy.sh --skip-build 192.168.70.89
	scripts/deploy.sh --skip-build 192.168.70.92

.PHONY: deploy-yaml
deploy-yaml:  ## 仅推 gateway.yaml + 重启  GATEWAY_YAML=./path/to/yaml
	@test -f $(GATEWAY_YAML) || (echo "GATEWAY_YAML not found: $(GATEWAY_YAML)" && exit 1)
	$(SCP) $(GATEWAY_YAML) $(NODE):/tmp/gateway.yaml.new
	$(SSH) 'sudo install -m0640 /tmp/gateway.yaml.new /etc/leap/gateway.yaml && \
	  sudo systemctl restart leap-gateway && rm -f /tmp/gateway.yaml.new && \
	  sleep 1 && sudo journalctl -u leap-gateway -n 5 --no-pager'

.PHONY: deploy-gvisor
deploy-gvisor: build-linux  ## 推 gvisor TUN 配置到 89（per_terminal 前置）
	scripts/deploy.sh --config /tmp/gw89-gvisor.yaml --skip-build 192.168.70.89

.PHONY: deploy-perterm
deploy-perterm: build-linux  ## 推 per_terminal:true 配置到 89
	scripts/deploy.sh --config /tmp/gw89-perterm.yaml --skip-build 192.168.70.89

.PHONY: deploy-full
deploy-full: stage  ## 全量重装 (changed install.sh / *.service / nft templates)
	$(SCP) build/leap-stage.tgz $(NODE):/tmp/
	$(SSH) 'cd /tmp && rm -rf leap-stage && tar --no-same-owner -xzf leap-stage.tgz && \
	  sudo bash leap-stage/install.sh'

# ---------------------------------------------------------------------------
# Observe
# ---------------------------------------------------------------------------

.PHONY: status
status:  ## GET /api/status
	@curl -s --max-time 5 $(LEAP_API)/api/status | python3 -m json.tool

.PHONY: health
health:  ## GET /api/proxies/active
	@curl -s --max-time 5 $(LEAP_API)/api/proxies/active | python3 -m json.tool

.PHONY: nodes
nodes:  ## GET /api/nodes/health (probe + passive stats)
	@curl -s --max-time 5 $(LEAP_API)/api/nodes/health | python3 -m json.tool

.PHONY: logs
logs:  ## tail leap-gateway 日志
	$(SSH) 'sudo journalctl -fu leap-gateway --no-pager'

.PHONY: mihomo-logs
mihomo-logs:  ## tail leap-mihomo 日志
	$(SSH) 'sudo journalctl -fu leap-mihomo --no-pager'

.PHONY: ssh
ssh:  ## 直接 ssh 进节点  NODE=dianwei@<ip>
	@ssh -o StrictHostKeyChecking=no -o ConnectTimeout=8 $(NODE)

.PHONY: nft
nft:  ## 查看 leap nft table
	$(SSH) 'sudo nft list table inet leap'

# ---------------------------------------------------------------------------
# One-time bootstrap (run once per node before anything else)
# ---------------------------------------------------------------------------

.PHONY: setup-ssh
setup-ssh:  ## ★ 一次性推公钥 + NOPASSWD sudo（先跑！）  NODE=dianwei@<ip>
	./scripts/setup-ssh.sh $(NODE)

.PHONY: clean
clean:  ## 清理 build 产物
	rm -rf build/
