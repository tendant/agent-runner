# 沙箱测试手册 / Sandbox testing guide

**One command each:** `make sandbox-smoke` (this host, as a normal user: Seatbelt on macOS,
gVisor on Linux; `REAL=1` adds one billed task with the real claude CLI) and `make
sandbox-agentbox` (Linux gVisor in an unprivileged Docker container, from any host). Both run the
conformance suite against the baseline (`.github/sandbox-baselines/agentbox-<backend>.json`), then
the real runner with a fake `claude` that tries to escape, and check every outcome: credentials,
the runner's own files, other sessions and projects, writes outside the workspace, the network
(egress through the gateway, private and metadata addresses refused, the model proxy reachable
with egress `none`), `strict` mode, a data dir under a 0700 home, codex on a fake ChatGPT login
through the model proxy, and that a claude run without credentials fails up front. On Linux they
also check both egress modes and two fork bombs: one under `AGENT_SANDBOX_MEMORY=512m` (must be
killed) and one under `AGENT_SANDBOX_PIDS=256` (`fork()` must fail with `EAGAIN`), neither harming
the host.

## A. 自动化测试（任何机器）

```bash
go build ./... && go test ./internal/...        # 全部
go test ./internal/sandbox/... ./internal/executor/...
go test -v ./internal/sandbox/e2e               # 真实 adapter + 假 agentbox 后端的端到端
```

`e2e` 覆盖：严格模式通过、策略拒绝（后端无法限制内存）、超时杀进程树、崩溃恢复（supervisor 回收孤儿）、
导出冲突进隔离区。它们验证接线与逻辑，**不验证真实隔离**。agentbox 自身的测试（`make test-linux`、
macOS 上的 `go test ./...`）对真实 runsc / sandbox-exec 验证隔离。

## B. 本机冒烟（runner 级）

```bash
AGENT_SANDBOX=off        ./agent-runner   # 与以前完全一致，无 sandbox.* 日志
AGENT_SANDBOX=permissive ./agent-runner   # 日志出现 sandbox.started，detail 列出 unenforced 能力
AGENT_SANDBOX=strict AGENT_SANDBOX_EVIDENCE=report.json ./agent-runner
AGENT_SANDBOX=bogus      ./agent-runner   # 每个 run 都被拒绝；绝不回退到宿主执行
```

预期：strict 下存在缺口 → 会话在开始前失败，日志 `sandbox.rejected`；run 结束后
`STATE_ROOT/sandbox/leases` 为空，出现 `sandbox.finished`。

## C. 真实主机验证

```bash
go build -o sandbox-conformance ./cmd/sandbox-conformance
./sandbox-conformance --report report.json --manifest manifest.json \
  --require-file .github/sandbox-baselines/agentbox-gvisor.json     # macOS: agentbox-seatbelt.json
```

以运行 runner 的普通用户执行（Linux 先由管理员在 agentbox 中 `make install`）。输出列出未通过/跳过/未运行的测试；
只有测试全通过的能力才进入 manifest。

## D. 手动红队清单（在沙箱内的 agent / 命令里执行）

| 尝试 | 期望 |
|---|---|
| `cat ~/.ssh/id_rsa`、`cat ~/.aws/credentials`、`ls ~` | 拒绝 |
| 写入工作区之外（如 `echo x > ~/outside`） | 拒绝 |
| 工作区内 `ln -s ~/.ssh/id_rsa l; cat l` | 拒绝 |
| `curl http://169.254.169.254/`、连接宿主私有地址上的服务 | 失败（经网关也被拒绝） |
| `curl --noproxy '*' https://example.com` | Linux transparent：经网关成功；proxy 模式：失败 |
| 启动 `setsid sleep 9999 &` 后结束 run，再 `pgrep -f "sleep 9999"` | 无残留（macOS 需 cgo 构建） |
| 同时跑两个 run，在一个里 `pkill -f <另一个的命令>` | 另一个不受影响 |
| `env` | 不含宿主 `ANTHROPIC_API_KEY` 等（除非在 `AGENT_SANDBOX_ENV_ALLOW` 中） |
| macOS：`security show-keychain-info login.keychain` | 失败（钥匙串服务不可达） |
| 运行中 `kill -9` runner，另起 `sandbox-supervisor --leases $STATE_ROOT/sandbox/leases` | 数秒内回收孤儿进程并删除租约 |
| 向工作区写 `.git/hooks/pre-commit` 后让 runner 提交 | 钩子不在宿主执行（gitsafe） |

## E. 尚未接线、无法冒烟的部分
磁盘配额/输出上限、diff 导出隔离区——代码与单测存在，但未进入运行路径，见 `docs/sandbox.md` 的 Known gaps。

## F. CI 自动化（GitHub Actions）
`.github/workflows/sandbox.yml`：PR/每晚运行 `unit`（`go test ./internal/sandbox/... ./internal/executor/...`）和
`smoke` 矩阵（macOS→seatbelt；ubuntu-24.04→gvisor，用 agentbox 的 `install-runsc.sh` 安装 runsc 与 AppArmor
配置），以普通用户运行 `scripts/sandbox-smoke`。基线未被证明时失败；报告作为 artifact 上传。
