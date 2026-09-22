# 验证报告 — req_d67df3b2f16c2b85 (nova install 203/EXEC 修复)

**执行时间**: 2026-09-22
**执行环境**: macOS Darwin arm64 (F1s-MacBook-Pro.local, kernel 24.3.0)
**工作目录**: `/Users/f1/.novaworkbench/worktrees/novaworkbench/req_d67df3b2f16c2b85`
**任务定位**: 纯验证，不修改任何源码/配置。

---

## 0. 环境就绪性核对（执行前必检）

| 项 | 期望 | 实际 | 结论 |
|---|---|---|---|
| OS | Linux | `Darwin … arm64` | ❌ 非 Linux |
| systemd | 存在 | `which systemctl` → not found | ❌ 无 systemd |
| sudo | 可用 | uid=501(f1), admin 组 | ⚠️ 有 sudo 但无目标二进制 |
| `claude` CLI / `node` / `go` | 安装 | go1.25.0 已装 | ✅ |
| nfpm | 已装或可装 | `/opt/homebrew/bin/nfpm` v2.47.0 | ✅ |
| `dpkg-deb` | 已装 | `not found` | ❌ 无 |
| `/usr/bin/nova` | 应存在 | `No such file or directory` | ❌ |
| `/etc/systemd/system/nova.service` | 应存在 | `No such file or directory` | ❌ |
| 源码前置条件 (Go 修复已落地) | 满足 | `canonicalInstallPath` 引用 14 次;`servicemgr_linux_test.go` 存在;`nfpm.yaml` 含 `mode:` 1 次 | ✅ |

> **环境定性**：本机是 macOS 开发机，**没有** Linux + systemd + `/usr/bin/nova` 三个本任务硬性依赖。本任务只能在 darwin 上做"编译验证 / schema 验证"两件事，**e2e systemd 验证必须在具备 systemd 的 Ubuntu 测试机/VM 上重做**。

---

## A. 复现 + 验证修复（systemd e2e）

### 状态：⏭ 跳过 / 未执行

**跳过原因（原始输出）**：

```
$ uname -a
Darwin F1s-MacBook-Pro.local 24.3.0 … arm64

$ which systemctl
07.  systemctl not found

$ ls -l /usr/bin/nova
ls: /usr/bin/nova: No such file or directory
```

**结论**：步骤 A 必须在 Ubuntu（或任何 Linux + systemd + sudo）测试机上执行。本机 darwin 不具备。

**为推进验证所做的离线等价检查**（**仅证明代码逻辑方向，不替代真实 systemd 测试**）：

A1. 读 `backend/internal/servicemgr/servicemgr_linux.go` 确认 Install() 在 ExecStart 解析块（`opts.ExecStart = exe`）**之后**有 `ensureBinaryInstalled(opts.ExecStart)` 调用，并紧跟 `opts.ExecStart = canonicalInstallPath`。

A2. 读 helper 实现确认：源路径相同只走 chmod 分支；源路径不同走 `os.Open` + `OpenFile(tmp,".new",0755)` + `io.Copy` + `Close` + `os.Rename` + `os.Chmod(0755)`；任何步骤失败都会带 context 返回 error；全部 `os.Remove(tmp)` 兜底。

A3. 单测覆盖（见 D 节编译结果）—— helper 与 renderUnit 行为已断言。

---

## B. 幂等性（`sudo nova install` 二次运行）

### 状态：⏭ 跳过 / 未执行

**跳过原因**：同 A —— 需 Linux + systemd 环境。

**离线等价分析**：helper 在 source==canonicalInstallPath 时走 chmod-only 分支；二次 install 时 `os.Executable()` 解析 `opts.ExecStart = /usr/bin/nova`，helper 命中 same-path 分支 → 仅 `os.Chmod(canonicalInstallPath, 0o755)`。幂等性由 helper 设计与单测断言共同保证。

---

## C. apt 路径（nfpm + deb mode）

### 状态：⚠️ 部分验证 / 发现 1 处 FAIL

### C.1 当前仓库 nfpm.yaml 语法尝试构建 deb —— **失败**

**实际命令与输出**：

```bash
$ nfpm package --packager deb -f /Users/f1/.novaworkbench/worktrees/novaworkbench/req_d67df3b2f16c2b85/packaging/nfpm.yaml -t /tmp/nova-current.deb
…
ERROR
Yaml: unmarshal errors: line 52: field mode not found in type files.Content.

$ ls -la /tmp/nova-current.deb
ls: /tmp/nova-current.deb: No such file or directory
```

**最可能原因**：nfpm v2.47 的 YAML schema 不接受 content 项顶层 `mode:` 字段。正确的位置是嵌套在 `file_info.mode` 下，且为整数（`0755`）而非字符串（`"0755"`）。

**Schema 证据**（`nfpm jsonschema` 输出）：

```json
"Content": {
  "properties": {
    "src": …, "dst": …, "type": …,
    "file_info": { "$ref": "#/$defs/ContentFileInfo" },
    "expand": …
  },
  "additionalProperties": false,
  "required": ["dst"]
},
"ContentFileInfo": {
  "properties": {
    "owner": …, "group": …,
    "mode": { "type": "integer" },   ← 这里，不是顶层
    "mtime": …
  }
}
```

**影响**：当前 `packaging/nfpm.yaml` 的修复（顶层 `mode: "0755"`）在 nfpm v2.47 上**直接被拒绝**。如该仓库交付物走 nfpm 打包，那么**apt 路径上的 `/usr/bin/nova` 模式位并不会被这个改动锁定为 0755**——意味着只要用户走 `apt install` 而后**没有再跑** `sudo nova install`，systemd 203/EXEC 循环**仍会复现**。

### C.2 正确语法 + 实际构建验证 —— 通过

**实际命令与输出**：

```bash
$ cat > /tmp/test-nfpm-fixed.yaml <<'EOF'
…
contents:
  - src: /tmp/fake-nova
    dst: /usr/bin/nova
    expand: true
    file_info:
      mode: 0755
umask: 0077
EOF
$ nfpm package --packager deb -f /tmp/test-nfpm-fixed.yaml -t /tmp/nova-fixed.deb
using deb packager...
created package: /tmp/nova-fixed.deb
$ ls -la /tmp/nova-fixed.deb
-rw-r--r--@ 1 f1  wheel  654  9 22 11:43 /tmp/nova-fixed.deb

$ cd /tmp && ar x nova-fixed.deb && tar -tvf data.tar.gz | grep usr/bin/nova
-rwxr-xr-x  0 root   root       39  9 22 11:43 ./usr/bin/nova
```

**结论**：改用 `file_info: { mode: 0755 }` 后，deb 内的 `/usr/bin/nova` 模式位为 **0755**（`-rwxr-xr-x`），apt 路径可执行性兜底**在正确语法下**是有效的。

**`dpkg-deb -c` 替代验证**：本机未装 `dpkg-deb`，但 `ar x + tar -tvf` 等价于 `dpkg-deb -c`（deb 本质是 ar 归档 + data.tar.*）。结论等价。

---

## D. 单元测试回归

### 状态：⚠️ 编译通过 / 运行不可达

**实际命令与输出**：

```bash
$ cd backend && go test -count=1 ./internal/servicemgr/...
?   	github.com/novaworkbench/backend/internal/servicemgr	[no test files]
```

```bash
$ cd backend && GOOS=linux GOARCH=amd64 go test -count=1 ./internal/servicemgr/...
fork/exec /var/folders/.../servicemgr.test: exec format error
FAIL	github.com/novaworkbench/backend/internal/servicemgr	0.001s
```

**分析**：

1. **首次尝试（darwin 主机，GOOS 默认 darwin）**：测试文件首行 `//go:build linux`，整个测试文件被排除 → 报 `[no test files]`。这是预期（darwin 主机不应用 Linux 专属符号），**不能**解读为"测试不存在"。
2. **二次尝试（GOOS=linux 跨平台编译 + 运行）**：编译成功（说明 helper / renderUnit 测试代码本身无语法/类型错误），但 darwin kernel 无法执行 Linux ELF 测试二进制 → `exec format error`。这是预期。
3. **真正回归**必须在 Linux 上跑：
   ```bash
   cd backend && go test -count=1 ./internal/servicemgr/...
   ```

**结论**：测试代码存在、跨编译通过，但因本机 darwin 不带 Linux kernel，**实际断言未执行**。需在 Linux 上重跑。

---

## E. 额外发现（验证过程的副产品）

### E.1 当前工作树未包含 `server` 二进制

任务说明假设工作目录里有现成的 `novaworkbench-linux-amd64` 二进制用于 A/B 节 install。本 worktree 内**没有**该二进制（仅有 `verify-nvm` 脚本和 `run.sh`）；其他 worktree 的 `server` 二进制是 darwin host target（`go build` 默认产物），不能在 Linux 上直接执行。如需在 Ubuntu 测试机上跑 A/B 节，需先：

```bash
make build           # 输出 dist/novaworkbench-linux-amd64
```

### E.2 `nfpm.yaml` 修复需要修正语法（已在 C.1 节详述）

**推荐 patch**（仅做核对展示，不在本验证任务中执行修改）：

```yaml
contents:
  - src: dist/novaworkbench-linux-${ARCH}
    dst: /usr/bin/nova
    expand: true
    file_info:                    # 嵌套对象
      mode: 0755                  # 整数，不要引号
```

> 这超出本验证任务边界（任务说明明确"纯验证任务：不要修改任何源码或配置文件"），仅作为结论性发现提交给后续修复人。

---

## 总体结论

| 步骤 | 是否可执行 | 本机结论 | 必须复查环境 |
|---|---|---|---|
| A. systemd e2e | ❌ 不可执行 | 跳过 | Ubuntu + systemd + sudo |
| B. 幂等性 | ❌ 不可执行 | 跳过 | 同上 |
| C. apt 路径 | ✅ 部分 | **当前 nfpm.yaml 语法错误，apt 路径修复未生效；正确语法下可保证 0755** | Ubuntu + dpkg/apt 装包后 ls -l 复核 |
| D. 单测回归 | ⚠️ 编译过/运行不可达 | 跨编译 OK；需 Linux 实际跑断言 | Linux + go1.25 |

### 修复是否生效 —— **分场景结论**

- **`sudo nova install` 路径（场景 1）**：代码静态检查 OK，单测编译 OK；但**本环境无法 e2e 验证**。
- **`apt install nova` 路径（场景 2）**：**当前仓库的 `packaging/nfpm.yaml` 修复无效**（顶层 `mode: "0755"` 被 nfpm v2.47 schema 拒绝，build 直接失败）。如不修正，apt 安装后 `/usr/bin/nova` 仍可能落到非 0755 mode，**用户必须额外跑 `sudo nova install` 才能闭环**——这对直接走 apt 的用户是回归。

### 最终建议

1. **在 Ubuntu 测试机上重做 A/B 节 e2e**（含 `chmod 0644 /usr/bin/nova` 后 `sudo ./nova install` 的回归用例），记录 `journalctl -u nova -n 20`。
2. **修正 `packaging/nfpm.yaml`**：把顶层 `mode: "0755"` 改成嵌套 `file_info: { mode: 0755 }`；同步更新 L53-L56 注释。
3. **修正后重做 C 节**：用 `nfpm package` 构建 deb，`ar x data.tar.*` 或在 Ubuntu 上 `dpkg-deb -c` 校验 `/usr/bin/nova` mode=0755。
4. **在 Ubuntu 上重做 D 节**：`cd backend && go test ./internal/servicemgr/...` 期望 PASS。

**验证报告就绪**。