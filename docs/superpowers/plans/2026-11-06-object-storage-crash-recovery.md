# 对象存储崩溃残留可恢复性根治 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) 或 superpowers:executing-plans 逐任务实施本计划。步骤用 `- [ ]` 复选框跟踪。

**Goal:** 让"monibuca 进程崩溃后，对象存储未上传的录像分片能被自动定稿并补传"这件事在生产配置下真正生效——当前 196 现场因为一个配置键大小写不匹配 + 扫描目录为空，恢复机制静默失效。

**Architecture:** 三处收口：① 恢复任务读取 tempdir 时按键名归一化（与 `config.Parse` 同一规则），消除"写了配置读不到"；② 恢复扫描目录不再静默为空，启动日志打印**实际**扫描集合、空集合并按配置升级为启动失败；③ S3 未显式配置 tempdir 时不再静默回落系统 `/tmp`，给出可操作告警并支持强制要求。配套修正 196 实例配置与 xde-installer 模板/部署校验，最后用"录制中 kill -9"和"上传中停 minio"两个真机场景验收。

**Tech Stack:** Go 1.26（module `m7s.live/v5`）、gotask 任务系统、gopkg.in/yaml.v3 配置解析、MinIO（S3 兼容）、Ansible（xde-installer，独立 GitLab 仓库）。

**Spec:** GitHub issue eanfs/monibuca#23（`ops(mp4): 196 未配置 storage.s3.tempdir，对象存储崩溃残留无法自动恢复`）+ 本文件「背景」中的实测证据。issue 标题中的"未配置"需在实施时更正为"配置键名不匹配导致读取失败"。

## 背景（实测证据，2026-11-06 196 现场）

| 观察 | 结论 |
|---|---|
| 196 `monibuca-config.yaml` 写了 `storage.s3.tempDir: /monibuca/record` | 配置**存在** |
| 启动日志 `WRN storage.s3.tempdir is not configured` | `recovery_index.go` 的 `mediaTempDir()` **读不到** |
| 启动日志 `index recovery task started ... dirs=[]` | 打印的是配置值 `t.Dirs`，非实际扫描集合（误导） |
| 11-05 18:12 的残留落在 `/monibuca/record` 且带 `.m7sidx` | 写入端 `effectiveTempDir()` 生效（走 `config.Parse`，会归一化键名） |
| 11-06 10:10 的 13 个残留落在 `/monibuca/tmp`（系统 tmp）且**无 sidecar** | v5.3.7 尚无 TempDir/索引机制；这些残片按用户决定丢弃 |
| its-server 每 2 分钟对 31 个 `rec-<cam>-<ms>.mp4` 报 `获取文件元数据失败` | 重启打断录制后无对象；24h 慢查后自动分流 |

根因三层：
1. **键名大小写不一致**：`mediaTempDir()` 用硬编码 `conf["tempdir"]` 读**原始** YAML map（键为 `tempDir`），而写入端走 `config.Parse`（归一化后为 `tempdir`）。同一份配置两条路径结论相反。
2. **扫描集合覆盖不到落盘目录**：`defaultDirs()` = tempdir + pending_uploads + 录像输出目录。196 直传 S3、无本地录像目录，tempdir 又因 (1) 为空 → 集合只剩 `pending_uploads`。
3. **失败静默**：空集合只 Warn 一条，启动照常；日志打印配置值而非实际集合，运维看不出"恢复功能已废"。

## 目标 / 非目标

**目标**
- 配置键 `tempDir` / `tempdir` / `temp_dir` 三种写法都能被恢复任务读到。
- 启动日志能一眼看出"实际扫描哪些目录、每个目录为何在/不在"。
- 扫描集合为空时按配置**拒绝启动**，不再带病上线。
- S3 启用但 tempdir 未配置时，行为可解释（告警 + 可选强制）。

**非目标**
- 不扫描系统 `/tmp`（安全论证见 `recovery_index.go` 注释：全局可写目录可被投毒 sidecar）。
- 不抢救 11-06 10:10 的 13 个无 sidecar 残片（用户已决定丢弃）。
- 不改 `pending_uploads` 的上传重试算法（已有实现，本计划只验证其与恢复的交互）。
- 不改 its-server（xde-media-spring-boot-starter）。

## Global Constraints

以下约束对所有任务生效，违反即验收不通过：

- **TDD**：先写失败测试再实现；测试必须能在修复前失败、修复后通过（计划里给出"破坏性验证"步骤）。
- **task 系统**：`Run()` 内联跑在父 Job 事件循环上，禁止阻塞；禁止裸 goroutine，用 `AddTask`/`Go()`；`OnStart` 必须在 `AddTask` 之前注册；`SetRetry(非零,…)` 必须配 `SetMaxRetryInterval`。
- **日志**：slog 键值对 `t.Info("msg","key",value)`；不新增 `fmt.Printf`/`log.Printf`（`pkg/storage/upload_manager.go` 既有 `log.Printf` 不动）。
- **配置**：新增键必须 YAML 全小写、带 `default:` 与 `desc:`；既有公开键不得破坏；`storage.s3` 段既有键名（`tempDir` 驼峰）必须**继续可用**（兼容现场配置，不要求运维立刻改名）。
- **build tag**：`pkg/storage/s3.go` 受 `s3` tag 门控；`plugin/mp4` 不得直接 import `S3StorageConfig`（保持现有解耦，靠 `map[string]any`）。
- **门禁**：每个任务结束 `gofmt -w <changed-files>`、`go vet <affected-packages>`、`go test <affected-packages> -count=1` 全绿；涉及并发的加 `-race`。
- **不改无关文件**；不顺手重构 `recovery_index.go` 既有函数。

## Review Focus

以下 5 类输入/失败模式 spec 未逐条规定、但最可能坑到使用者，每类都在对应任务里钉了测试：

1. **配置键三种写法**（`tempDir` / `tempdir` / `temp_dir`）：使用者会按文档或旧模板写成驼峰，读不到就静默失去恢复能力 → Task 1 测试三种写法。
2. **`storage` 段存在但只有 `local`**：不能误报"S3 未配置"或"tempdir 未配置"，否则噪音掩盖真问题 → Task 1/4 测试"无 s3 键时不报"。
3. **tempdir 指向不存在的目录**（容器忘记挂载）：录制写入失败与恢复扫描失败的报错必须可区分且指明路径 → Task 2 测试"目录不存在时列在不可用项"。
4. **`recovery.dirs` 与 tempdir 不一致**（只配了 dirs 漏了 tempdir）：新残留仍扫不到 → Task 2 测试"tempdir 不在 dirs 内时显式 Warn 并补入"。
5. **上传失败进 pending 后，index 恢复再次处理同一文件**：可能重复上传/对象覆盖 → Task 7 真机用"停 minio 制造失败"验证最终只有一份对象且 pending 清空。

---

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `plugin/mp4/recovery_index.go` | 恢复任务：目录收集、sidecar 发现、日志与校验 | 修改 |
| `plugin/mp4/recovery_index_tempdir_test.go` | tempdir 键归一化与目录集合的单测 | 新建 |
| `plugin/mp4/index.go` | MP4 插件配置结构（新增 `recovery.requiredirs`） | 修改 |
| `pkg/storage/s3.go` | S3 tempdir 解析与"未配置"告警/强制 | 修改 |
| `pkg/storage/s3_tempdir_test.go` | 既有 tempdir 测试，补充"未配置告警/强制"用例 | 修改 |
| `CLAUDE.md` | Common Pitfalls：恢复三要素与键名陷阱 | 修改 |
| `docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md` | 部署清单补 tempdir 校验 | 修改 |
| xde-installer `roles/monibuca/templates/monibuca-config-v5.yaml.j2` | 配置模板键名与恢复段 | 修改（跨仓库） |
| xde-installer `roles/monibuca/defaults/main.yml` | tempdir/dirs 变量 | 修改（跨仓库） |
| xde-installer `roles/monibuca/tasks/main.yml` | 部署后 fail-fast 校验 | 修改（跨仓库） |
| xde-installer `roles/monibuca/templates/docker-compose-xde-monibuca-v5.yml.j2` | 持久卷挂载路径 | 修改（跨仓库） |

---

### Task 1: tempdir 配置键归一化读取

**Files:**
- Modify: `plugin/mp4/recovery_index.go`（`mediaTempDir()` 及新增 `normalizeConfigKey`）
- Test: `plugin/mp4/recovery_index_tempdir_test.go`

**Interfaces:**
- Consumes: 无（本任务为首个任务）
- Produces:
  - `func normalizeConfigKey(k string) string` —— 小写、删除 `-` 与 `_`（与 `pkg/config` `ParseUserFile` 的键归一化完全一致）
  - `func (t *IndexRecoveryTask) mediaTempDir() string` —— 语义不变，但遍历 `ServerConfig.Storage["s3"]` 的所有键做归一化匹配；未命中返回 `""`

- [ ] **Step 1: 写失败测试**

```go
// plugin/mp4/recovery_index_tempdir_test.go
func TestMediaTempDirAcceptsAllKeySpellings(t *testing.T) {
    cases := []struct{ name string; conf map[string]any; want string }{
        {"camel tempDir", map[string]any{"s3": map[string]any{"tempDir": "/data/tmp"}}, "/data/tmp"},
        {"lower tempdir", map[string]any{"s3": map[string]any{"tempdir": "/data/tmp"}}, "/data/tmp"},
        {"snake temp_dir", map[string]any{"s3": map[string]any{"temp_dir": "/data/tmp"}}, "/data/tmp"},
        {"blank value", map[string]any{"s3": map[string]any{"tempDir": "   "}}, ""},
        {"no s3 section", map[string]any{"local": "."}, ""},
    }
    for _, c := range cases { /* 构造 IndexRecoveryTask{plugin: &MP4Plugin{Plugin: m7s.Plugin{Server: &m7s.Server{ServerConfig: ...Storage: c.conf}}}}，断言 mediaTempDir() == c.want */ }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./plugin/mp4 -run TestMediaTempDirAcceptsAllKeySpellings -count=1 -v`
Expected: FAIL — `camel tempDir` 与 `snake temp_dir` 两例得到 `""`（当前只认 `tempdir`）

- [ ] **Step 3: 实现**

在 `recovery_index.go` 增加 `normalizeConfigKey`（复用 `strings`，三行），`mediaTempDir()` 改为遍历 map 键、归一化后等于 `"tempdir"` 即取值并 `TrimSpace`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./plugin/mp4 -run TestMediaTempDirAcceptsAllKeySpellings -count=1 -v`
Expected: PASS（5 个子例全绿）

- [ ] **Step 5: 破坏性验证（证明测试有效）**

把 `mediaTempDir()` 临时改回硬编码 `conf["tempdir"]`，重跑 Step 4 命令，Expected: FAIL；改回修复版，Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add plugin/mp4/recovery_index.go plugin/mp4/recovery_index_tempdir_test.go
git commit -m "fix(mp4): tempdir 配置键归一化读取，修复 tempDir 写法读不到导致恢复失效"
```

---

### Task 2: 恢复扫描目录可见性与空集合告警

**Files:**
- Modify: `plugin/mp4/recovery_index.go`（`Start()` 日志、新增 `describeRecoveryDirs()`）
- Test: `plugin/mp4/recovery_index_tempdir_test.go`（同文件追加）

**Interfaces:**
- Consumes: Task 1 的 `mediaTempDir()`
- Produces:
  - `func (t *IndexRecoveryTask) describeRecoveryDirs() (dirs []string, missing []string)` —— `dirs` 为实际存在的扫描目录（`defaultDirs()` 过滤掉不存在的），`missing` 为配置/推断出但目录不存在或不可写的路径
  - `Start()` 日志键：`"dirs", dirs`、`"missingDirs", missing`、`"tempdir", t.mediaTempDir()`

- [ ] **Step 1: 写失败测试**

断言三件事：① `describeRecoveryDirs()` 在 `tempdir` 不存在时把它归入 `missing`；② `tempdir` 已配置但不在 `Dirs` 时，`dirs` 仍包含它（补入语义）；③ `dirs` 与 `missing` 同时为空时 `Start()` 返回错误（见 Task 3 的开关，默认 false 时改为 `Error` 日志）。

```go
func TestDescribeRecoveryDirsMarksMissingTempDir(t *testing.T) { /* tempdir=/nonexistent → missing 含该路径, dirs 不含 */ }
func TestDescribeRecoveryDirsIncludesTempDirWhenNotInDirs(t *testing.T) { /* Dirs=[] 且 tempdir 存在 → dirs 含 tempdir */ }
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./plugin/mp4 -run TestDescribeRecoveryDirs -count=1 -v`
Expected: FAIL — `describeRecoveryDirs` 未定义（编译失败）

- [ ] **Step 3: 实现**

`defaultDirs()` 保持返回候选集合；新增 `describeRecoveryDirs()` 用 `os.Stat` 分类。`Start()` 把 `"dirs", t.Dirs` 改为打印实际集合 + `missingDirs` + `tempdir`；集合为空时 `t.Error(...)` 并写明"崩溃残留不会被自动恢复"。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./plugin/mp4 -run 'TestDescribeRecoveryDirs|TestMediaTempDir' -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add plugin/mp4/recovery_index.go plugin/mp4/recovery_index_tempdir_test.go
git commit -m "fix(mp4): 恢复扫描目录打印实际集合并对空集合显式告警"
```

---

### Task 3: `recovery.requiredirs` 启动强制校验

**Files:**
- Modify: `plugin/mp4/index.go`（`RecordConfig.Recovery` 增字段）、`plugin/mp4/recovery_index.go`（`Start()` 返回错误）
- Test: `plugin/mp4/recovery_index_tempdir_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 的 `describeRecoveryDirs()`
- Produces:
  - 配置键 `mp4.record.recovery.requiredirs`（`bool`，`default:"false"`）
  - `func validateRecoveryDirs(dirs, missing []string, required bool) error` —— `required && len(dirs)==0` 时返回 `fmt.Errorf("recovery scan dirs empty: %v", missing)`；否则 `nil`

> **注意**：`IndexRecoveryTask` 是 `p.AddTask(recoveryTask)` 注册的（未 `WaitStarted`），因此 `Start()` 返回 error **不会**让插件启动失败。校验必须放在 **`MP4Plugin.Start()` 注册 recoveryTask 之前**调用 `validateRecoveryDirs`，由插件启动失败来阻断。

- [ ] **Step 1: 写失败测试**

```go
func TestValidateRecoveryDirs(t *testing.T) {
    cases := []struct{ name string; dirs, missing []string; required bool; wantErr bool }{
        {"required and empty", nil, []string{"/data/tmp"}, true, true},
        {"required with dirs", []string{"/data/tmp"}, nil, true, false},
        {"optional and empty", nil, []string{"/data/tmp"}, false, false},
    }
    // 断言 validateRecoveryDirs(...) != nil 与 wantErr 一致；required 失败时 error 文案含 "recovery scan dirs empty"
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./plugin/mp4 -run TestValidateRecoveryDirs -count=1 -v`
Expected: FAIL — `validateRecoveryDirs` 未定义

- [ ] **Step 3: 实现**

`index.go` 的 `Recovery` struct 增加 `RequiredDirs bool `default:"false" desc:"恢复扫描目录为空时是否拒绝启动（防止恢复能力静默失效）"``；`MP4Plugin.Start()` 在 `p.AddTask(recoveryTask)` **之前**调用 `dirs, missing := recoveryTask.describeRecoveryDirs()` 与 `validateRecoveryDirs(dirs, missing, p.RecordConfig.Recovery.RequiredDirs)`，返回 error 即中止插件启动。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./plugin/mp4 -run TestValidateRecoveryDirs -count=1 -v`
Expected: PASS

- [ ] **Step 5: 配置校验回归**

Run: `go test ./plugin/mp4 -count=1`
Expected: 全包 PASS（确认新增字段未破坏既有默认配置解析）

- [ ] **Step 6: Commit**

```bash
git add plugin/mp4/index.go plugin/mp4/recovery_index.go plugin/mp4/recovery_index_tempdir_test.go
git commit -m "feat(mp4): recovery.requiredirs 在扫描目录为空时拒绝启动"
```

---

### Task 4: S3 未配置 tempdir 的可解释行为

**Files:**
- Modify: `pkg/storage/s3.go`（`S3StorageConfig` 增 `RequireTempDir`；`NewS3Storage` 校验）
- Test: `pkg/storage/s3_tempdir_test.go`

**Interfaces:**
- Consumes: 无
- Produces: 配置键 `storage.s3.requiretempdir`（`bool`，`default:"false"`）；当 `true` 且 `TempDir` 为空时 `NewS3Storage` 返回 `fmt.Errorf("%w: storage.s3.tempdir is required", ErrInvalidStorageConfig)`
  - 校验放在 `validateAndNormalizeEndpoint()`（`NewS3Storage` 只调这个入口，**不是** `Validate()`），tempdir 判断置于 accessKey/bucket 校验之后

- [ ] **Step 1: 写失败测试**

```go
func TestNewS3StorageRequiresTempDirWhenConfigured(t *testing.T) {
    cfg := &S3StorageConfig{Endpoint: "127.0.0.1:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", RequireTempDir: true}
    if _, err := NewS3Storage(cfg); err == nil || !strings.Contains(err.Error(), "tempdir is required") { t.Fatalf("got %v", err) }
}
func TestNewS3StorageAllowsEmptyTempDirByDefault(t *testing.T) {
    // RequireTempDir=false → 不因 tempdir 为空报错（既有行为不回退）
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -tags s3 ./pkg/storage -run TestNewS3StorageRequiresTempDir -count=1 -v`
Expected: FAIL — `RequireTempDir` 字段不存在

- [ ] **Step 3: 实现**

字段 + `Validate()` 中校验（保持 `NewS3Storage` 现有校验顺序，tempdir 校验放在 bucket 之后）。

- [ ] **Step 4: 运行确认通过**

Run: `go test -tags s3 ./pkg/storage -run TestNewS3Storage -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/storage/s3.go pkg/storage/s3_tempdir_test.go
git commit -m "feat(storage): storage.s3.requiretempdir 强制要求配置 tempdir"
```

---

### Task 5: 文档与运行手册

**Files:**
- Modify: `CLAUDE.md`（Common Pitfalls 增一条）
- Modify: `docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md`（部署清单）

**Interfaces:**
- Consumes: Task 1–4 的配置键名与日志键
- Produces: 文档

- [ ] **Step 1: CLAUDE.md 增条目**

内容要求（写清楚三点，不写实现细节）：① 恢复三要素 `mp4.record.index.enable` + `storage.s3.tempdir` + `mp4.record.recovery.dirs`；② **`tempDir`/`tempdir`/`temp_dir` 都能读，但模板统一小写**；③ 启动日志 `index recovery task started` 的 `dirs` 必须是实际存在的目录列表，为空即恢复失效（`requiredirs: true` 会拒绝启动）。

- [ ] **Step 2: runbook 增部署校验清单**

条目：容器内 `test -w <tempdir>`；`tempdir` 必须在 `recovery.dirs` 内；启动日志不得出现 `tempdir is not configured`。

- [ ] **Step 3: 校验文档命令可复制执行**

Run: `grep -n "requiredirs" CLAUDE.md docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md`
Expected: 两个文件都命中

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md
git commit -m "docs: 记录对象存储崩溃恢复三要素与部署校验"
```

---

### Task 6: xde-installer 模板与部署校验（跨仓库，GitLab MR target=`develop`）

**Files:**
- Modify: `roles/monibuca/templates/monibuca-config-v5.yaml.j2`
- Modify: `roles/monibuca/defaults/main.yml`
- Modify: `roles/monibuca/tasks/main.yml`
- Modify: `roles/monibuca/templates/docker-compose-xde-monibuca-v5.yml.j2`

**Interfaces:**
- Consumes: Task 1–4 的配置键（`tempdir`、`mp4.record.recovery.requiredirs`、`storage.s3.requiretempdir`）
- Produces: 模板变量 `monibuca_s3_tempdir`（默认 `/monibuca/tmp`）、`monibuca_recovery_dirs`（默认 `["/monibuca/tmp", "/monibuca/pending_uploads"]`）

- [ ] **Step 1: 模板键名统一**

`monibuca-config-v5.yaml.j2`：`tempDir: "/monibuca/record"` → `tempdir: "{{ monibuca_s3_tempdir }}"`；`storage.s3` 段增 `requiretempdir: true`；新增 `mp4.record` 段：

```yaml
mp4:
  enable: true
  record:
    index:
      enable: true
    recovery:
      onstart: true
      requiredirs: true
      dirs: {{ monibuca_recovery_dirs | to_json }}
```

- [ ] **Step 2: 卷挂载与 tempdir 对齐**

`docker-compose-xde-monibuca-v5.yml.j2`：把 `./monibuca/tmp:/tmp` 改为 `./monibuca/tmp:{{ monibuca_s3_tempdir }}`，避免系统 `/tmp` 语义（容器重启可能被清、与本机其它进程共享）。

- [ ] **Step 3: 新增部署后 fail-fast 校验 task**

`roles/monibuca/tasks/main.yml` 追加（渲染完成后、启动容器后）：

```yaml
- name: 校验 monibuca 对象存储崩溃恢复配置
  shell: |
    set -e
    test -n "{{ monibuca_s3_tempdir }}"
    docker exec xde-monibuca sh -c 'test -w {{ monibuca_s3_tempdir }}'
    for i in $(seq 1 15); do docker logs xde-monibuca 2>&1 | grep -q 'index recovery task started' && break; sleep 2; done
    docker logs xde-monibuca 2>&1 | grep -q 'index recovery task started'
    ! docker logs xde-monibuca 2>&1 | grep -q 'tempdir is not configured'
  register: monibuca_recovery_check
  changed_when: false
  failed_when: monibuca_recovery_check.rc != 0
```

- [ ] **Step 4: 本地渲染验证（不执行远端）**

Run: `ansible-playbook --syntax-check <site.yml>` 与 `ansible-lint roles/monibuca/`
Expected: 语法/规范检查通过；渲染后的配置里 `tempdir` 非空且 `dirs` 含该路径

- [ ] **Step 5: Commit（xde-installer 仓库，MR target`develop`）**

```bash
git add roles/monibuca/
git commit -m "feat(monibuca): 固化对象存储 tempdir 与崩溃恢复配置校验"
```

---

### Task 7: 真机验收（196）

**Files:** 无代码改动，产出验收记录（追加到本文件末尾或 issue #23 评论）

**Interfaces:**
- Consumes: Task 1–6 全部产物；v5.3.14+ 镜像
- Produces: 验收结论（通过/不通过 + 证据）

- [ ] **Step 1: 部署新镜像并确认配置生效**

启动日志必须出现 `index recovery task started` 且 `dirs` 含 tempdir 与 pending；不得出现 `tempdir is not configured`。

- [ ] **Step 2: 异常中断复测（崩溃定稿）**

对一路流 `POST /mp4/api/start/<path>`（`fragment: 30s`）→ 录制 20s → `docker kill -s KILL xde-monibuca` → 重启。
Expected: 重启日志出现 `index recovery` 扫描到该 sidecar，`.m7sidx` 经 `.recovering` 消失；MinIO 出现 `rec-<cam>-<ms>.mp4`。

- [ ] **Step 3: 上传恢复能力复测**

`docker stop xde-minio` → 录制中 kill monibuca → 重启 monibuca（minio 仍停）→ 观察文件进入 `/monibuca/pending_uploads`；`docker start xde-minio` → 等待重试。
Expected: pending 目录最终清空，对象出现在 MinIO，且**同一 path 只有一份对象**（无重复上传覆盖）。

- [ ] **Step 4: 负向验证**

临时删掉 `tempdir` 配置重启。
Expected: `requiredirs/requiretempdir` 为 true 时**启动失败**并给出明确 error；设为 false 时启动成功但日志有 `Error` 级告警。

- [ ] **Step 5: 记录验收结果**

把三次测试的时间线、命令、日志片段写进 issue #23 评论；本计划末尾补一行结论。

---

## 自审

| 检查项 | 结论 |
|---|---|
| Spec 覆盖 | 根因 1（键名）→ Task 1；根因 2（集合覆盖）→ Task 2；根因 3（静默失败）→ Task 2/3；写入端告警 → Task 4；部署固化 → Task 6；验收 → Task 7。issue #23 的"验收"两条（不再出现 WRN、残留能被 scanned>0）分别落在 Task 3 Step 1 与 Task 7 Step 2。 |
| 步骤粒度 | 每步一个可检查动作；破坏性验证（Task 1 Step 5）证明测试不是摆设。 |
| 类型/命名一致 | `mediaTempDir`、`normalizeConfigKey`、`describeRecoveryDirs`、`RequireTempDir`、`RequiredDirs` 在 Task 1–4 间一致。 |
| Review Focus | 5 条各自有测试：①Task 1 Step 1；②Task 1 用例 `no s3 section` + Task 4 默认 false 用例；③Task 2 用例 `MarksMissingTempDir`；④Task 2 用例 `IncludesTempDirWhenNotInDirs`；⑤Task 7 Step 3。 |
| 比例 | 本计划约 250 行，spec（issue #23 + 实测证据）约 100 行；无整段函数体转录。 |
