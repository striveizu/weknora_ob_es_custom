# WeKnora ES8 + OceanBase 4.4.2：实施与验证交接

> 本文保留交接时状态；“尚未提交”描述的是该快照时点。仓库内文档链接已调整为可移植路径，实际提交以 Git 历史为准。

交接日期：2026-10-14（Asia/Shanghai）。按用户要求停止后续验证，余下工作移交同事；本文是停止时的事实快照。

## 1. 当前结论与接手范围

第一版 PoC 已实现 OceanBase MySQL 模式的主数据库接线、独立建表迁移、Wiki Repository SQL 适配和任务队列事务适配。真实 OceanBase 上的 **6 组集成测试通过**，已启动的后端上 **18 次 Wiki／登录相关 HTTP 检查通过**。这些结果支持继续推进该方案。

**自动 Wiki 的完整导入生成链路尚未验证；也未验证真实 ES8 的分块写入与检索。当前版本不是生产验收完成状态。** ES8 服务已启动，后端选择 `elasticsearch_v8` 驱动，但不能将“服务可启动”认定为检索业务通过。Wiki 页面搜索目前仍由 OceanBase 执行。

代码目录：`<仓库根目录>`。

源码基线：`94fae6de43a36e44d64aecbbdf19c0b72b352aa8`。所有实现修改仍在工作区，**尚未 git commit、push 或创建 PR**；包含新增未跟踪文件。移交时必须传递整个修改集，不能只传 Git HEAD 或 `git diff` 默认输出。

先前分析与本版说明：

- [可行性技术报告](技术分析报告-WeKnora-ES8-OceanBase4.4.2.md)
- [智能体实施任务书](智能体实施任务书-WeKnora-ES8-OceanBase4.4.2.md)
- [代码依赖清单](代码依赖清单.json)
- [PoC 实现和操作说明](../docs/architecture/oceanbase-poc.md)

## 2. 已通过的验证

### 2.1 真实 OceanBase 集成测试：通过

版本由测试查询确认：`5.7.25-OceanBase_CE-v4.4.2.1`。使用真实 MySQL 兼容租户；创建独立随机测试库，完成测试后删除该测试库。最终测试耗时约 22 秒，包含迁移和清理；这不是吞吐或生产性能测量。

| 测试组 | 已断言的行为 |
| --- | --- |
| 初始化及重复迁移 | 新库 baseline 执行成功；再次执行迁移成功；schema 版本、dirty 状态校验成功；测试库清理成功 |
| `UserDefaultsAndDatabaseAssignedSequences` | 用户 JSON preferences 写入 `{}`；chunk／tag 非主键自增序号在 DB 内生成，正确回填调用者 |
| `JSONSourcesLongTextAndSearch` | 超过 64 KiB 的中文正文读回一致；来源 ID 精确与 `ID\|标题` 前缀查询；含 `%`、`_` 的 ID 不发生通配误匹配；跨库页面搜索、分类过滤、标题归一化、相似候选、orphan 计数 |
| `ConcurrentVersionSnapshotAndDeleteRecreate` | 同版本两个并发更新只成功一个，另一个返回版本冲突；只留下一个修订快照；存活 slug 唯一约束；软删除后可同 slug 重建 |
| `GroupedClaimsLateSiblingAndFailureCounters` | 两个并发认领者取得不同文档组，每组含同文档的两条 op；已认领组的晚到 sibling 不被再次认领；8 个并发失败计数增量不丢失 |
| `GuardedSeedAndDrain` | 文档 finalizing 状态与 pending op 原子播种；drain 后文档恢复 completed |
| `FolderDeleteRecreateAndMigrationGuards` | 存活目录唯一约束、删除后重建；dirty schema 拒绝验证／迁移；遗留 migration owner 拒绝继续迁移 |

注意：上述并发测试是单进程多个 goroutine 对真实 DB 的并发事务，**不是多机器 Worker 故障测试**。目录级联删除的深树及所有目录移动场景也没有因此被覆盖。

证据：[集成测试完整通过日志](交接验证证据-2026-10-14/oceanbase-integration.log.txt)。源码：`internal/application/repository/oceanbase_integration_test.go`。

### 2.2 本地相关回归与编译：通过，但范围有限

最新的定向回归命令：

```bash
docker exec weknora-ob-poc-build go test -p 2 \
  ./internal/application/repository ./internal/database ./internal/types \
  ./internal/container ./internal/application/repository/retriever/elasticsearch/v8 \
  ./internal/application/service \
  -run 'Test.*(Wiki|StorageBackend|VectorStore|MySQLDSN|StringArray)' -count=1
```

相关包通过；**该命令中的 ES8 包显示 `[no tests to run]`**，只能说明编译通过，不能用于 ES 运行时验收。没有传 OceanBase 测试 DSN 时，真实 DB 集成测试会 skip；DB 真实验收以 2.1 的独立运行结果为准。

另外，一次未加名称过滤的指定包回归中，Repository、database、types、container、ES8 Repository 包通过；service 包失败，详情见下一节。并未执行整个仓库 `go test ./...`。

应用 Linux arm64 二进制编译成功，路径 `WeKnora/build/WeKnora-ob-poc`。最新后端已重建容器并启动，包含 MySQL `key` 列引用修复和 30 秒查询超时参数。此前执行 `git diff --check` 没有输出；后续没有继续执行新的验收。

证据：[定向回归日志](交接验证证据-2026-10-14/oceanbase-focused-tests.log.txt)。

### 2.3 实际 HTTP 接口：通过

使用隔离 PoC 后端、真实 OceanBase、合成用户身份执行 `scripts/oceanbase_http_smoke.py`，最后一次运行成功。

| 行为 | 实际结果 |
| --- | --- |
| 既有合成用户登录 | 200，取得会话 |
| 创建 Wiki 知识库 | 201，关闭 vector／keyword／graph，开启 Wiki |
| 创建目录、创建页面、读取页面 | 201／201／200 |
| 按当前版本修改页面 | 200 |
| 再用旧版本修改 | 409，符合预期 |
| 修订历史 | 200，并断言至少一个快照 |
| 列表查询、目录、index、graph、stats、search、lint | 全部 200 |
| 删除页面后同 slug 重建 | 204／201 |

成功验收知识库 ID：`0d25993a-4704-4687-a55a-6b048f5c8198`。保留了页面 `entity/oceanbase-poc` 和 `concept/recreate` 供检查。此前脚本试跑还留下过一个合成知识库，属于 PoC 测试数据。

HTTP 脚本对多数读接口验证的是状态码，修订历史额外验证了快照数量；搜索结果内容的强断言在 Repository 集成测试中完成。**没有做浏览器点击验收，也没有测试图谱内容完整性。** UI 首页和经 UI 代理访问 auth/config 曾返回 200。

注册接口不能列为“已通过”：早期一次注册返回 400，根因日志为用户插入达到 10 秒 OB 查询超时；资源与超时调整后验证了登录，但未另建新邮箱重复验证注册成功响应。不要依据“可以登录”补写注册验收通过。

证据：[HTTP 执行日志](交接验证证据-2026-10-14/oceanbase-http.log.txt)、[结构化检查结果](交接验证证据-2026-10-14/oceanbase-http-evidence.json)。

## 3. 出现过的失败及处置

| 问题 | 已采取措施 | 交接状态 |
| --- | --- | --- |
| 低资源 OB 连续 DDL／DML 延迟、迁移 deadline、用户插入超时 | 当前容器内存上限升至 7 GiB，OB memory_limit=6G，test resource unit=3G；应用 DSN ob_query_timeout=30 秒；迁移独立使用更长超时 | 调整后完整 OB 集成测试通过；生产资源、长时间稳定性未验证 |
| OB JSON／TEXT 非空默认值不兼容 | baseline 去掉不受支持的默认值；GORM Create 回调把模型 JSON 默认值显式写入，克隆本条 statement schema，避免改共享缓存 | 用户默认值真实测试通过；所有模型／map Create 场景未穷尽 |
| chunk／tag 的非主键自增序号未回填 | 创建后按 UUID 查询 DB 分配的 seq_id，tag 在同一事务回填 | 真实 DB 测试通过 |
| 系统配置查询 `key` 保留字 | 通过 `clause.Column` 引用条件和排序列 | 已编译并运行修复；系统配置完整 CRUD 未专门验收 |
| 后端缺失 gojieba 词典 | 从当前 Go module v1.4.7 复制字典到 build，并只读挂载到编译时 module 路径 | 后端启动及 HTTP 测试通过 |
| HTTP 冒烟脚本读取 204 空响应时报 JSONDecodeError | 支持空 body；再次运行 | 最终完整脚本通过 |
| service 包广泛回归失败 | 未修改 SSRF 防护；保留失败日志 | 未解决，见下文 |

广泛 service 回归中，`example.com` 解析为 `198.18.0.18`，`api.e2b.app` 解析为 `198.18.0.20`，被既有 SSRF 检查拒绝，导致 MCP 与 E2B sandbox 一批测试失败。日志支持“本机 DNS／代理环境干扰”这一判断，但**未在修复 DNS 后重跑，也未对原始基线复现作最终归因**；因此不能声称完整回归全绿或所有失败都已排除为环境原因。交接后应先处理测试网络环境，再重新判断。

证据：[失败摘录](交接验证证据-2026-10-14/service-regression-failures.txt)。原完整日志留在 `WeKnora/build/oceanbase-regression.log`。

## 4. 尚需验证／实现的工作，按优先级接手

### P0：自动 Wiki 核心验收

1. **导入→解析／分块→pending op→Wiki map/reduce→页面／来源→completed 全流程。** 停止前只阅读了模型、导入和 Wiki prompt 代码；没有建立模拟模型服务，也没有配置真实模型。可先用本地可控模型响应验证链路，再使用真实合成模型验证输出质量；两类结论分开记录。
2. **增量更新、删除资料与撤回。** 同一文档再次导入，确认旧事实更新、source_refs 一致、历史保留；删除来源后检查引用清理、仍有其他来源的页面以及孤立页面。
3. **多 Worker 与故障恢复。** 至少两个后端／Worker 实例，模拟抢占、崩溃、重试、晚到 op、旧 Worker 迟到提交；检查无丢任务、无错误覆写及最终状态收敛。现有 stale claim 保留约 90 分钟机制，未新增 owner fencing／续租，不能宣称 exactly-once。
4. **注册与权限。** 新用户注册、workspace 归属、共享 KB、不同角色、API key 范围及跨租户拒绝；尤其覆盖自动 Wiki 背景任务所用租户上下文。

### P1：ES8 与数据库兼容完整性

5. **真实 ES8 文档索引／检索。** 当前只确认服务版本和驱动选择，需测试 keyword、vector、过滤、中文召回、更新／删除／重建、索引命名及跨租户隔离。原向量实现仍为 script_score，未引入 HNSW。
6. **Wiki 专用 ES 索引／Outbox：尚未实现。** 当前 Wiki SQL 字面搜索和 Go 分页 bigram 相似候选可用于 PoC；若目标是 Wiki 也由 ES 检索，应另行实现投影、幂等索引、版本、重建和权限回读，并验证双写失败恢复。
7. **SQL／schema 审查。** baseline 为 72 张现有应用表加 `task_claim_lanes`，迁移器另建 2 张管理表。需逐模块检查 PG 专有 SQL、列宽／NULL／JSON 默认值、索引与约束、时间精度、JSON Scan、全部系统配置 CRUD。建表成功不等于所有表业务兼容。
8. **迁移运维。** 失败／进程崩溃留下 owner 的恢复演练，dirty schema 的人工修复流程，后续版本升级。现有 baseline 只支持新安装；更改 baseline 不会升级已部署的 version=1 库，不能自动 force-clean。现有 PG 数据迁移尚未实现。
9. **完整回归与环境复现。** 修复上述 DNS 干扰，重跑 service 全包及其他相关包；在同事机器从空卷启动，验证脚本不是仅本机可运行。

### P2：生产前验证与范围扩展

10. 数据规模增长、并发导入、长文、深目录、多租户、OB／ES／Redis 重启、网络中断、备份恢复、持续运行与压测；形成资源参数和验收阈值。
11. 素材图库触发器未移植，`chunk_images` 表虽然存在，但素材投影不能视为受支持。OAuth、外部数据源、memory、可选工具等尚未完整回归。
12. 不启用自建 Langfuse 的 PostgreSQL 栈；未将 Langfuse 改成 OB。第三方组件与企业准入需独立确认。

## 5. 代码入口

| 文件／目录（均相对 WeKnora） | 作用 |
| --- | --- |
| `internal/container/container.go` | mysql 主库初始化、迁移与 schema gate、驱动约束 |
| `internal/database/oceanbase.go` | DSN、GORM JSON 默认值、baseline 迁移、owner／dirty 管理 |
| `cmd/oceanbase-migrate/` | 独立初始化命令 |
| `migrations/oceanbase/` | 嵌入 baseline SQL、schema-manifest、版本常量 |
| `scripts/generate_oceanbase_baseline.py` | 从 SQLite catalog＋PG JSON 声明生成新安装 schema；未搬迁旧数据 |
| `internal/application/repository/wiki_page.go`、`wiki_page_mysql.go` | Wiki JSON／搜索／相似候选兼容 |
| `internal/application/repository/task_queue.go` | 按 lane 行锁认领、drain、失败计数、文档状态事务 |
| `internal/application/repository/chunk.go`、`tag.go` | 非主键序号回填 |
| `internal/application/repository/system_setting.go` | `key` 列引用修复 |
| `internal/types/session.go` | StringArray Scan 兼容 nil／string／[]byte |
| `internal/application/repository/oceanbase_integration_test.go` | 真实 DB 验收 |
| `scripts/oceanbase_poc.py` | 隔离容器、凭证、建库账号、迁移、编译、测试、启停 |
| `scripts/oceanbase_http_smoke.py` | 合成用户与 Wiki HTTP 验收；会创建并保留新的验收 KB，不调用模型 |

其他 MySQL 适配涉及 model_usage、mcp_metadata、session、user、organization、storagebackend、vectorstore；以工作区完整 diff 为准。

## 6. 本机环境与凭证交接

停止新增验证后，以下 PoC 容器保持运行，便于同事接手；没有停止原有 WeKnora，也未改其 PostgreSQL 数据。

| 服务 | 本机地址／说明 |
| --- | --- |
| UI | http://127.0.0.1:18081 |
| 后端 | http://127.0.0.1:18080；`/health` 曾验证 200 |
| OceanBase | `127.0.0.1:12881`，MySQL 模式；DB=`weknora_poc`，user=`weknora_poc@test` |
| ES8 | http://127.0.0.1:18920，8.19.6；本机 PoC 关闭 ES 安全认证 |
| Redis | 仅 Docker 网络可达，有独立生成密码 |
| Docker 网络 | `weknora-ob-poc` |
| Go builder | `weknora-ob-poc-build`，Go 1.26.8 Linux arm64／CGO |

当前 OB：镜像 `oceanbase/oceanbase-ce:4.4.2.1-101000022026050611`，单节点；容器上限 7 GiB、memory_limit=6G、system_memory=1G、test unit=3G（meta／user 拆分）、cpu_count=8、test log_disk=2G。这是资源受限本机配置，建议复现机器 Docker VM 至少 12 GiB，生产另定。

私密文件均位于 `WeKnora` 根目录，权限 0600，并被 Git 忽略。**本文和证据不复制密码／JWT，以下私密文件通过安全渠道单独交接即可获得实际密码：**

- `.env.oceanbase-poc`：应用数据库的 `DB_PASSWORD`、Redis 密码、JWT_SECRET 和运行参数。
- `.env.oceanbase-poc-admin`：本地 tenant 管理与测试账户密码。
- `.env.oceanbase-poc-tests`：真实 DB 测试 DSN；测试用户只获准操作 `weknora_ob_test_<随机值>` 库。
- `.env.oceanbase-poc-login`：合成 UI 用户 `ob-poc@example.invalid` 的 `POC_PASSWORD`。
- `.oceanbase-poc-session.json`：临时会话，通常无需转交，可自行登录获取。

OB 与文件存储已有独立持久卷。**当前运行的 ES／Redis 是早期创建的容器，没有数据卷挂载**；脚本的新建配置已加入持久卷，但 `infra` 复用现有容器，不会自动补挂载。停启保留容器层，删除重建会丢失这两个服务现有数据；接手前按需要安排迁移。勿直接删除数据库／文件卷。

builder 默认依赖本机已有 `weknora-app-builder:auto-register` 镜像；跨机器需准备具有 Go 1.26 和 CGO 依赖的镜像并指定 `POC_GO_IMAGE`。前端和 docreader 复用已有发布镜像，未按本次后端 HEAD 重编，界面版本兼容需验收。docreader 当前使用 AMD64 镜像模拟运行于 arm64。

## 7. 同事可使用的命令（本次未继续执行）

进入代码目录：

```bash
cd /path/to/weknora_ob_es_custom
```

现有环境恢复启动：

```bash
python3 scripts/oceanbase_poc.py infra
python3 scripts/oceanbase_poc.py app
```

新环境顺序：`credentials` → `infra` → 等待 test 租户 ready → `accounts` → `migrate` → `build` → `app`。不要对已初始化且有数据的库自动删除重建。密码变更必须同步数据库账户与环境文件。

同事决定继续验收后：

```bash
python3 scripts/oceanbase_poc.py test
python3 scripts/oceanbase_http_smoke.py
```

前者创建／清理随机测试库；后者保留合成用户和验收知识库。日志请存新文件，保留本文快照证据。

源码改动后的编译／后端替换：

```bash
python3 scripts/oceanbase_poc.py build
docker stop weknora-ob-poc-app
docker rm weknora-ob-poc-app
python3 scripts/oceanbase_poc.py app
```

只有 `build` 不会热替换已经运行的后端。只处理带 `weknora-ob-poc-` 前缀的测试容器；原有 `WeKnora-*`、`bodhi2-mcp` 不属于本次环境。

结束 PoC：

```bash
python3 scripts/oceanbase_poc.py stop
```

停止保留卷和凭证，不负责删除数据。所有验证证据已复制到本报告旁的 `交接验证证据-2026-10-14/`，可与本 Markdown 一起转交；原始构建日志、二进制和词典在 Git 忽略的 `WeKnora/build/`。

## 8. 建议接手验收结论格式

每一项记录：源码版本／diff、OB／ES 版本与资源、输入资料、模型（模拟或真实）、Worker 数量、操作步骤、预期、实际、日志／DB／索引证据、清理情况。没有执行的项目标“未验证”，环境阻断标“阻断”，失败保留失败；避免把单测、HTTP 200、服务启动或模型模拟混同为自动 Wiki 全流程通过。
