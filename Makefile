# Makefile — leap-gateway dev / deploy targets.
#
# Quick reference:
#   make help           # list targets
#   make build          # local build for darwin
#   make test           # go test ./...
#   make deploy-fast    # 仅传 leap-gateway 二进制 + 重启 leap-gateway (~10s)
#   make deploy-singbox # 改 sing-box 二进制本身才需要 (rare; pin 在 1.10.7)
#   make deploy-full    # 改 install.sh / *.service / nft 模板才需要 (~2-3min)
#   make stage          # 打 build/leap-stage.tgz 不部署
#   make logs           # tail leap-gateway 日志
#   make singbox-logs   # tail leap-singbox 日志
#   make status         # 拉一次 /api/proxies/active
#
# Prereq (运行一次): make setup-ssh — 推 ed25519 公钥 + 装 NOPASSWD sudoers dropin.
# 之后所有 make 目标都不再需要交互输入密码。

NODE        ?= dianwei@192.168.70.92
NODE_HOST   := $(word 2,$(subst @, ,$(NODE)))
LEAP_API    ?= http://192.168.70.92:18080
GATEWAY_YAML?= ./gateway.yaml
GOOS        ?= linux
GOARCH      ?= amd64
LDFLAGS     := -s -w -X main.Version=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

export GATEWAY_YAML

SSH_OPTS    := -o StrictHostKeyChecking=no -o ConnectTimeout=8
SSH         := ssh -o BatchMode=yes $(SSH_OPTS) $(NODE)
SCP         := scp $(SSH_OPTS)

.DEFAULT_GOAL := help

.PHONY: help
help:  ## 列出所有 target
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build:  ## 本地构建（macOS dev 用）
	go build -o build/leap-gateway ./cmd/gateway

.PHONY: build-linux
build-linux:  ## 交叉编译 linux/amd64 二进制（部署用）
	@mkdir -p build
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags='$(LDFLAGS)' \
	  -o build/leap-gateway ./cmd/gateway
	@ls -lh build/leap-gateway | awk '{print "  leap-gateway", $$5}'

.PHONY: test
test:  ## go test ./...
	go test ./...

.PHONY: vet
vet:  ## go vet
	go vet ./...

.PHONY: verify-render
verify-render:  ## 用 sing-box 1.10.7 校验渲染产物 schema (sing-box check)，预防 grpc-bug 类回归
	@./scripts/verify-render.sh

.PHONY: stage
stage:  ## 打全量部署 tarball (build/leap-stage.tgz, ~14M gzipped)
	./scripts/stage.sh

# ---------------------------------------------------------------------------
# Deploy
# ---------------------------------------------------------------------------

.PHONY: deploy-fast
deploy-fast: build-linux  ## ⚡ 仅热替换 leap-gateway 二进制 + 重启服务 (~10s)
	@echo "[deploy-fast] scp leap-gateway → $(NODE_HOST)"
	@$(SCP) build/leap-gateway $(NODE):/tmp/leap-gateway.new
	@echo "[deploy-fast] install + restart"
	@$(SSH) 'sudo install -m0755 /tmp/leap-gateway.new /usr/local/bin/leap-gateway && \
	  sudo systemctl restart leap-gateway && \
	  rm -f /tmp/leap-gateway.new && \
	  sleep 1 && sudo journalctl -u leap-gateway -n 5 --no-pager'
	@echo "[deploy-fast] done — $(LEAP_API)"

.PHONY: deploy-singbox
deploy-singbox:  ## 替换 sing-box 二进制本身 (pin 1.10.7)，**会断海外 5-10s**
	@test -f build/leap-stage/sing-box || (echo "run 'make stage' first to fetch sing-box" && exit 1)
	@echo "[deploy-singbox] scp sing-box → $(NODE_HOST)"
	@$(SCP) build/leap-stage/sing-box $(NODE):/tmp/sing-box.new
	@$(SSH) 'sudo install -m0755 /tmp/sing-box.new /usr/local/bin/sing-box && \
	  sudo systemctl restart leap-singbox && rm -f /tmp/sing-box.new && \
	  sleep 2 && /usr/local/bin/sing-box version | head -2 && \
	  sudo systemctl is-active leap-singbox'

.PHONY: deploy-full
deploy-full: stage  ## 全量重装：改了 install.sh / *.service / nft 模板才用
	@echo "[deploy-full] scp tarball → $(NODE_HOST)"
	@$(SCP) build/leap-stage.tgz $(NODE):/tmp/
	@$(SSH) 'cd /tmp && rm -rf leap-stage && tar --no-same-owner -xzf leap-stage.tgz && \
	  sudo bash leap-stage/install.sh'

.PHONY: deploy-yaml
deploy-yaml:  ## 仅替换 /etc/leap/gateway.yaml 并重启 leap-gateway（不动二进制）
	@test -f $(GATEWAY_YAML) || (echo "no $(GATEWAY_YAML)" && exit 1)
	@$(SCP) $(GATEWAY_YAML) $(NODE):/tmp/gateway.yaml.new
	@$(SSH) 'sudo install -m0644 -o root -g root /tmp/gateway.yaml.new /etc/leap/gateway.yaml && \
	  sudo systemctl restart leap-gateway && rm -f /tmp/gateway.yaml.new && \
	  sleep 1 && sudo journalctl -u leap-gateway -n 5 --no-pager'

# ---------------------------------------------------------------------------
# Observe
# ---------------------------------------------------------------------------

.PHONY: status
status:  ## 拉一次 /api/proxies/active
	@curl -s --max-time 5 $(LEAP_API)/api/proxies/active | python3 -m json.tool

.PHONY: health
health:  ## 拉一次 /api/status
	@curl -s --max-time 5 $(LEAP_API)/api/status | python3 -m json.tool

.PHONY: logs
logs:  ## tail leap-gateway 日志
	@$(SSH) 'sudo journalctl -fu leap-gateway --no-pager'

.PHONY: singbox-logs
singbox-logs:  ## tail leap-singbox 日志
	@$(SSH) 'sudo journalctl -fu leap-singbox --no-pager'

.PHONY: ssh
ssh:  ## 直接 ssh 上节点（交互 shell）
	@ssh $(SSH_OPTS) $(NODE)

.PHONY: nft
nft:  ## 看节点上 leap nft table
	@$(SSH) 'sudo nft list table inet leap'

# ---------------------------------------------------------------------------
# One-time bootstrap
# ---------------------------------------------------------------------------

.PHONY: setup-ssh
setup-ssh:  ## 一次性：推公钥 + 装 NOPASSWD sudoers dropin (需要密码 ppio2026)
	@./scripts/setup-ssh.sh $(NODE)

.PHONY: clean
clean:
	rm -rf build/leap-stage build/leap-gateway build/leap-stage.tgz
