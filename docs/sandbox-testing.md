# 沙箱测试手册 / Sandbox testing guide

**One command each:** `make sandbox-smoke` (this host: Seatbelt on macOS, gVisor on Linux as root;
`REAL=1` adds one billed task with the real claude CLI) and `make sandbox-gvisor` (Linux gVisor in
a privileged Docker container, from any host). Both run the conformance suite against the CI
baseline, then the real runner with a fake `claude` that tries to escape, and check every outcome:
credentials, the runner's own files, other sessions, writes outside the workspace, the network,
and that a claude run without credentials fails up front. On gVisor they also run `strict` mode
and a fork bomb under `AGENT_SANDBOX_MEMORY=512m`, which must be killed without harming the host.

## A. 自动化测试（任何机器）

```bash
git checkout feat/sandbox
go build ./... && go test ./internal/...        # 全部
go test ./internal/sandbox/... ./internal/executor/...
go test -v ./internal/sandbox/e2e               # fake isobox 的端到端
```

`e2e` 覆盖：严格模式通过、策略拒绝（后端无法限制内存）、超时杀进程树、崩溃恢复（supervisor 回收孤儿）、
导出冲突进隔离区。`conformance` 套件约 1 分钟。它们验证接线与逻辑，**不验证真实隔离**。

## B. 本机冒烟（runner 级）

```bash
export AGENT_SANDBOX_ISOBOX=/path/to/isobox AGENT_SANDBOX_ENV_ALLOW=ANTHROPIC_API_KEY
AGENT_SANDBOX=off        ./agent-runner   # 与以前完全一致，无 sandbox.* 日志
AGENT_SANDBOX=permissive ./agent-runner   # 日志出现 sandbox.started，detail 列出 unenforced 能力
AGENT_SANDBOX=strict AGENT_SANDBOX_EVIDENCE=seatbelt-macos.json ./agent-runner
AGENT_SANDBOX=bogus      ./agent-runner   # 每个 run 都被拒绝；绝不回退到宿主执行
```

预期：strict 下存在缺口 → 会话在开始前失败，日志 `sandbox.rejected`；run 结束后
`STATE_ROOT/sandbox/leases` 为空，出现 `sandbox.finished`。

## C. 真实主机验证（关闭步骤 6 / 12）

```bash
git clone https://github.com/can1357/isobox && (cd isobox && go build -o isobox ./cmd/isobox)  # Go 1.26
go build -o sandbox-conformance ./cmd/sandbox-conformance

# macOS (Seatbelt)
./sandbox-conformance --isobox ./isobox/isobox --backend seatbelt \
  --report seatbelt-macos.json --manifest seatbelt-macos-manifest.json
# Linux：需要 root + cgroup v2 + iproute2/procps/iptables + runsc ≥ 20261005.0（release tarball，见 .github/workflows/sandbox.yml）
sudo ./sandbox-conformance --isobox ./isobox/isobox --backend gvisor \
  --report gvisor-linux.json --manifest production.json
```

输出会列出未通过/跳过/未运行的测试；只有测试全通过的能力才进入 manifest。
把两份 report JSON 回传即可记录证据。离线预览（任何主机）：
`ISOBOX_BIN=... ISOBOX_BACKEND=gvisor go test -run Real -v ./internal/sandbox/isobox`。

## D. 手动红队清单（在沙箱内的 agent / 命令里执行）

| 尝试 | 期望 |
|---|---|
| `cat ~/.ssh/id_rsa`、`cat ~/.aws/credentials` | 拒绝 |
| 写入工作区之外（如 `echo x > ~/outside`） | 拒绝 |
| 工作区内 `ln -s ~/.ssh/id_rsa l; cat l` | 拒绝 |
| `curl http://169.254.169.254/`、连接 `127.0.0.1` 上的宿主服务（egress=none） | 失败 |
| 启动 `setsid sleep 9999 &` 后结束 run，再 `pgrep -f "sleep 9999"` | 无残留（Seatbelt 可能残留：属已知弱点，应由套件显示为未证明） |
| 同时跑两个 run，在一个里 `pkill -f <另一个的命令>` | 另一个不受影响（Seatbelt 无 PID 隔离，预期失败） |
| `env` | 不含宿主 `ANTHROPIC_API_KEY` 等（除非在 `AGENT_SANDBOX_ENV_ALLOW` 中） |
| 运行中 `kill -9` runner，另起 `sandbox-supervisor --leases $STATE_ROOT/sandbox/leases` | 数秒内回收孤儿进程并删除租约 |
| 向工作区写 `.git/hooks/pre-commit` 后让 runner 提交 | 钩子不在宿主执行（gitsafe） |

## E. 尚未接线、无法冒烟的部分
受限出口（egress 网关）、模型代理、磁盘配额/输出上限、diff 导出隔离区——代码与单测存在，但未进入运行路径，见 `docs/sandbox.md` 的 Known gaps。

## F. CI 自动化（GitHub Actions）
`.github/workflows/sandbox.yml`：PR/每晚运行 `unit`（`go test ./internal/sandbox/... ./internal/executor/...`）和
`conformance` 矩阵（macOS→seatbelt，ubuntu+sudo→gvisor，安装 runsc 20250106.0，构建固定 ref 的 isobox）。
`sandbox-conformance --require-file .github/sandbox-baselines/<backend>.json` 在基线能力未被证明时以 exit 3 失败；
报告与 manifest 作为 artifact 上传。基线目前是保守初值：首次绿跑后收紧（gvisor 加入 process.* / resource.*）。
将 `ISOBOX_REF` 固定到 commit SHA 以获得可复现结果。托管 runner 若缺 cgroup v2 委派，resource.* 会不被证明，需自托管 Linux。
