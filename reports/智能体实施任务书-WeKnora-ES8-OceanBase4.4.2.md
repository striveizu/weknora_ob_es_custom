# 智能体实施任务书：WeKnora 自动 Wiki / ES8 / OceanBase 4.4.2

日期：2026-10-13。本文供后续编码智能体执行；当前交付只完成可行性分析，**任务尚未实现，也没有实机验证通过记录**。

## 0. 工作上下文

- 用户目标：保留 WeKnora 自动 Wiki，因公司数据库准入／信创要求，以 OceanBase 4.4.2 + ES8 替换应用 PostgreSQL 存储。
- 基线仓库：本目录的 `..`，commit `94fae6de43a36e44d64aecbbdf19c0b72b352aa8`。
- 先阅读：[完整技术报告](技术分析报告-WeKnora-ES8-OceanBase4.4.2.md) 和 [静态代码线索](代码依赖清单.json)。
- 设计假设：OB 为 MySQL 兼容租户；Redis、文件／对象存储保留。若发现实际仅有 Oracle 模式，暂停依赖 MySQL 的实现并报告差异。
- 未确认：OB 社区／企业版及 BP、OBProxy、ES8 小版本、实际数据量、是否有需迁移的历史数据。可先实现不依赖这些参数的代码与迁移设计；目标数据库验证需要实际实例。
- Go 模块要求 `go 1.26.0`，当前终端未找到 Go；先准备匹配的开发环境，不将未运行测试写成通过。

## 1. 固定架构与不变量

1. OceanBase 是业务事实来源；ES 是派生索引，Redis 是调度／协调层。
2. 所有 source chunks 保留在 OB；纯 Wiki 不要求向量索引。
3. 当前正文＋旧版本快照＋来源关系＋新增索引事件需在业务事务内保持一致。
4. 同一 dedup_key 的操作不得由两个有效 claim 同时处理；晚到同文档操作受已有 claim 约束。
5. 页面 CAS 失败必须重新加载与合并；不能覆盖先提交的更新。
6. 已删除文档／KB／租户不能因任务重试复活；删除后不能继续模型工作。
7. 活跃 slug／目录名的唯一性必须在 DB 中约束，允许合法删除重建。
8. 新 ES 事件必须使用独立 index_generation；页面 Version 不能代替索引代际。
9. 空权限 scope 不得解释为全库；共享 KB 按实际授权范围访问，不能只按调用者 tenant 过滤。
10. 保留 PostgreSQL／SQLite 现有行为与已有测试；mysql 分支必须显式化，未支持的 dialect 不静默 fallback。

## 2. 建议任务依赖

```text
T01 环境与兼容探针
  → T02 schema / migrator / 主库初始化
  → T03 核心方言与引用关系
  → T04 持久化队列与版本事务
  → T05 自动 Wiki 无 PG 端到端 PoC
  → T06 Wiki ES 搜索 + Outbox + 去重候选
  → T07 文档 ES 加固 / 可选 HNSW
  → T08 生产部署、升级与完整产品回归
```

T03 与 T04 的设计可提前准备，但 DB 契约在 T01/T02 后冻结。若未来由多个智能体实施，schema、类型、接口改动先由一个负责人合并，避免多个任务同时修改共享协议；是否启动并行智能体由后续执行者按用户授权与环境规则决定。

## T01：目标环境与能力探针（P0）

**产物：**`docs/architecture/oceanbase-capability-matrix.md`（建议未来路径），记录真实版本、补丁、SQL、返回值、执行计划与并发结果。

在隔离测试库确认：

- 连接、TLS、特殊字符密码、实际租户登录名、强一致读、parseTime、UTC。
- JSON 列参数／返回类型、SQL NULL、JSON null、`[]`、`{}`、中文和 emoji。
- 原样执行目标迁移中的 JSON 默认值、MEDIUMTEXT/LONGTEXT、DATETIME(6)、生成列、完整唯一键、长复合索引；不能只验证示例小表。
- AUTO_INCREMENT 非 UUID 主键的 seq_id 与批量 ID 回填；不假设 ID 连续。
- `FOR UPDATE`、必要 SHARE 或等价 UPDATE 锁、窗口函数、LIMIT 与 SKIP LOCKED 组合；两个连接保持事务同时执行。
- `GET_LOCK/RELEASE_LOCK` 与 OBProxy 会话生命周期；实际 golang-migrate Up、SetVersion 和 dirty 路径。
- READ COMMITTED／Serializable 的 BeginTx、写冲突错误码、死锁和回滚行为。
- ES `_license`、实际 server version、`search.allow_expensive_queries`、dense_vector dims 与 mapping、中文分析器、kNN 能力（如计划启用）。

可用的 JSON 探针示例（由测试工具绑定参数，或在隔离库手动执行）：

```sql
SELECT JSON_LENGTH('[]');
SELECT JSON_LENGTH('["甲", "乙"]');
SELECT JSON_CONTAINS('["doc-1"]', '"doc-1"');
SELECT JSON_UNQUOTE(JSON_EXTRACT('{"model_id":"m1"}', '$.model_id'));
```

**重要：**以上 SQL 通过不等于完整应用通过。锁测试至少包括：首 anchor 被锁后第二消费者能否取得其他 dedup_key、同 key 晚到 sibling、新／旧 claim 竞争、stale 回收、事务回滚与连接中断。

**退出标准：**每项标记 supported／adapted／unsupported／unverified；P0 不能遗留“unverified”。明确选择原 anchor 路线还是 claim 表路线，明确 migrator 的安全互斥方案。

## T02：完整 schema、迁移和启动接线（P0）

**主要文件：**`go.mod`、`internal/container/container.go`、`internal/database/migration.go`、新 `migrations/oceanbase/`、`internal/types/`、后续部署配置。

实施要求：

1. 加 `gorm.io/driver/mysql`，以 `DB_DRIVER=mysql` 支持 OB；保持 `Dialector.Name()=="mysql"`。
2. 建 DB capabilities/helper，至少表达 JSON 提取、数组长度、行锁、认领路线和迁移实现。不要以 `!=postgres` 代表 SQLite。
3. 新 baseline 对齐当前 schema。参考 PG 与 SQLite 迁移、types 及 startup 后处理，建立每张业务表／列／索引／投影的对照清单。
4. 明确仅剔除 PG retriever 的 embeddings／扩展／索引；基础用户、模型、Agent、RBAC、Wiki、审计、存储配置等必须齐全。
5. 处理 jsonb tags、自定义 Scan/Value、JSON 默认值、UTC 和大文本。StringArray.Scan 目前只接受 []byte，检查 OB 实际返回并避免失败时静默丢数组。
6. 部分唯一性用实测生成列方案或活跃键表；不得使用包含 NULL deleted_at 的简单 UNIQUE。
7. migrator 独立于 app Pod，应用校验 schema。失败需退出，不依赖 warning 后继续。
8. 生成列不能被 GORM 写入；不运行 AutoMigrate 去覆盖 explicit DDL。
9. baseline 记录源 commit、OB 迁移版本、升级规则和回滚策略；非事务 DDL 的 dirty 恢复不能简单 force 前一版本重跑。

**验收：**空 OB 库迁移后能启动基本应用；第二次迁移无副作用；两个部署迁移者不会并发执行；中途失败不会被报告为正常 schema；缺列／旧版 schema 的 app 不进入 ready。

## T03：核心 Repository 方言与来源关系（P0）

**主要文件：**`repository/wiki_page.go`、`chunk.go`、`model_usage.go`、`mcp_metadata.go`、`user.go`、`organization.go`、`temporary_document.go`、`chunk_image_assets.go`，以及相关类型与迁移。

实施要求：

- 转换 Wiki JSON 长度、空数组、路径精确比较、列表搜索、归一化标题查找、SearchAcross、来源反查。
- 把 Go 标题 identity 写入独立字段，并与现有 normalization 一致；更新、重命名、回填全部覆盖。
- 新增 `wiki_page_sources`／`wiki_page_chunk_refs`（推荐生产设计）；只开启一个权威写入路径，与原 JSON 同事务。先回填、校验，再切换读路径。
- PoC 暂未建立引用表时，支持纯 UUID 与 UUID|title，不能靠未解析的模糊匹配判定删除。
- 小规模去重用精确 identity＋分页轻量投影＋Go 相似度，报告该路径的扫描规模与性能边界；不能只禁用 FindSimilarPages。
- 精确保留分页/count/目录筛选、archived 排除和字段优先级；Go regex 合同与 ES 搜索合同单列。
- 全量审计所有 raw SQL 与 SQLite fallback。FAQ 已有 mysql 分支应复用，而 `json_each` 等必须处理。
- `model_usage.go` 的 OB JSON string 比较用 `JSON_UNQUOTE(JSON_EXTRACT(...))`，避免模型删除／使用检查失效。
- 素材库 `chunk_images` 的投影按目标版触发器能力实现；应用事务投影须覆盖上传、编辑、批量更新、复制与删除，不只首写。
- 确保 chunks／knowledge_tags seq_id 分配与来源 `cN` 等显示／工具链一致。

**验收：**CRUD、目录树、source read、删除撤销、统计、搜索特殊字符和归一化身份测试在 OB 上通过；未支持功能清单明确，不能仅以 UI 页面打开为完成。

## T04：队列、页面版本和生命周期事务（P0）

**主要文件：**`repository/task_queue.go`、`repository/wiki_page.go`、`repository/knowledge.go`、`service/wiki_ingest*.go`、`service/vectorstore.go`、`service/storagebackend.go`、`container/recover_pending_wiki_tasks.go`。

实施要求：

1. mysql 不落入 SQLite 的 ClaimBatch 分支。实现 T01 选择的 grouped anchor 或 claim table 算法。
2. ClaimBatch 的 limit 计 distinct dedup_key，同组 eligible op 一起认领；任何 fresh sibling 阻挡整组；优先 least-failed 再 FIFO。
3. 如果引入 owner/generation，接口、Schema、ack/release/delete/renew/stale recovery 同时升级，不留下无 owner 的旧 Worker 回调。
4. 改造 `IncrFailCount`、`DrainUnclaimedAndRelease` 的 RETURNING；维护不存在行与回滚行为。
5. finalizing 计数 seed 与 enqueue 原子，完成释放幂等，重启恢复不漏任务。
6. 保留 page expectedVersion CAS 与 snapshots，同事务管理新增来源表；不要把宽泛 IGNORE 当冲突处理。
7. OB 的 KB／租户存活检查、storage/vector store 绑定与删除使用正确行锁；建立 tenant→KB→knowledge/page 等经过分析的统一顺序。
8. 锁事务短于模型调用；事务重试有界、带退避。失败重试不要重复未授权模型调用。
9. 验证 Redis 故障、锁过期与旧 Worker 返回；对有效操作通过 DB CAS／claim owner 防护，不能只凭现有 best-effort slug lock。

**验收：**目标 OB、至少两个 app 实例和多个 Worker 上，通过同文档重复/晚到操作、不同文档同 slug 合并、Worker kill、stale 回收、删除竞争、死信与重启恢复。采用串行 KB 过渡路线时显式记录局限与退出计划。

## T05：自动 Wiki 无 PostgreSQL 端到端 PoC（P0 里程碑）

准备稳定 fixture：中文长文、包含同一实体的两份文档、重复名字、表格、图片引用、超64KiB页面、冲突信息、多层目录。先用模型 stub，再用真实合成模型。

覆盖：登录／创建 KB → 只开 Wiki → 上传解析 → 候选／引文／Reduce → Finalize → 页面／目录／图谱／引用回读 → 人工和 Agent 改写 → 版本历史／回滚 → 重解析与文档删除 → KB／租户删除。

部署没有 PG 容器、Service、DSN；检查 app、migrator、脚本与声明启用模块的日志。Redis 和文件存储正常保留。默认 Compose 的 PG depends_on 需真实解除，不能只“不启动 PG”。自建 Langfuse 首版明确关闭或单独部署。

**验收产物：**端到端报告，含 fixture、环境、命令、任务状态、页面／引用数量、故障恢复时间、真实 LLM token／费用，以及明确的已验证/未覆盖功能范围。

## T06：Wiki ES 索引与事务 Outbox（P1）

**新增组件建议：**`WikiSearchRepository`、ES Wiki mapping/template、`search_index_outbox`、`index_generation`、索引 Worker 和重建命令。名称为设计建议，当前仓库不存在这些完成品。

实施要求：

- ES 保存稳定 page_id `_id`，字段含来源ID、权限 scope、deleted 和 index_generation；JSON source refs 不直接复用为检索过滤字段。
- 页面变更、来源撤销、归档、重命名、删除、合并、链接或摘要变化等影响搜索字段的路径全部发事件。index_generation 与 Version 独立。
- 业务事务 commit 后消费 Outbox，按 OB 最新状态构造完整投影；bulk 逐项错误回写重试状态。
- 按 page owner/generation 协议处理乱序和重复；tombstone 防复活；分页重建的 checkpoint、别名切换与实时事件边界明确。
- 查询使用已授权 KB 列表且 fail-closed，回 OB 校验存活状态；跨租户共享 KB 使用资源所有者与授权 scope。
- 中文 title/aliases 精确＋ngram 候选＋Go 重排，保留现有 Go 去重安全规则；OB 精确匹配弥补 ES 刚写入的可见性延迟。
- 搜索列表、HTTP 搜索、Agent regex/literal 合同分别实现；不能以 ES regexp 宣称兼容整段文本 POSIX 正则。
- ES 宕机时 Wiki 写入能持久保存；检索与去重 fallback 具体可测，不能无限静默退化。
- 用户编辑立即回读 OB；必要搜索可见性路径用 wait_for 或 fallback；避免每篇批处理强制 refresh。

**验收：**断连恢复、乱序重放、重建中写入、删建同 slug、软删后迟到事件、metadata-only 更新、共享权限、召回质量和索引延迟测试通过。

## T07：文档 ES 检索加固与可选 HNSW（P1/P2）

**主要文件：**`repository/retriever/elasticsearch/structs.go`、`v8/repository.go`、`v8/move.go`、`container/engine_factory.go` 及 retriever 相关接口。

- 显式模板／mapping，稳定 `_id` 使用 tenant/KB/source_type/source_id/chunk_id 等能覆盖真实 IndexInfo 的完整业务身份；不要仅凭 chunk_id 假设所有索引类型唯一。
- 隔离模型、维度和 mapping 版本；建索引失败进入不可用状态，不仅打印日志。
- 文档移动／复制／删除／禁用／标签变化同步过滤字段，统一版本与重试幂等策略。
- 保留并复用已有 bulk 逐项失败检查和测试。
- 先评估 script_score 规模。选择 HNSW 时新增真实 kNN 查询，按授权 scope prefilter；重标定分数阈值并测试 Recall@K。
- keyword-only 无 embedding 不属于现有开箱能力，若需要，独立修改 embedding 要求、写入路径和 mapping。

**验收：**重复写不重复召回；不同维度／模型互不污染；move/copy/delete 保持权限与内容一致；向量分数／阈值有基准数据。

## T08：部署、升级、全产品回归（P2）

- OB 专用 Compose／Helm，支持公司现有外部 OB/ES、Redis、对象存储；固定镜像与版本，不用未验证 latest。
- migrator Job、防并发执行、schema gate、权限最小化、连接池与超时、备份恢复演练。
- 遗留 PG 数据离线迁移与 ES 重建（若有），保留 UUID、历史、作者、UTC、tenant 与 KB 边界。
- 跟进 memory 的 PG vector 分支与 Go fallback；RETRIEVE_DRIVER=ES 不会自动把长期记忆向量迁到 ES。根据规模关闭、保持 Go fallback 或另适配。
- 覆盖 MCP/OAuth、平台设置、审计、共享 Agent、素材库、数据源等实际启用模块；完成启用功能的 SQL 路径审计。
- 国产 CPU/OS 上验证 app、docreader/OCR、解析器、模型、镜像与全部外围组件；产品准入不是 DB 替换测试的同义词。
- 指标：DB 锁／事务重试、claim backlog/stale、finalizing、死信、LLM token、outbox backlog、ES lag、search latency、重建与回填校验失败。

## 3. 测试与配置交接

以下是取得匹配 Go 环境后的**建议命令**，本次未执行。先跑受影响包，再按新失败扩大范围；真实 OB 集成测试需新建测试并显式区分 skip 与 pass。

```bash
go test ./internal/application/repository
go test ./internal/application/service -run 'Test.*Wiki'
go test ./internal/application/repository/retriever/elasticsearch/v8
go test ./internal/database
```

OB 集成 DSN 变量可沿现有 PG migration test 模式设计，如未来 `WEKNORA_MIGRATION_TEST_OCEANBASE_DSN`；它目前不是已支持配置。检查测试是否真实命中 DB，不能让关键 PoC 仅靠 SQLite fixture。

完成代码后的示例环境（当前 commit 不能直接使用）：

```dotenv
DB_DRIVER=mysql
DB_HOST=<批准的OceanBase或OBProxy地址>
DB_PORT=<实际SQL端口>
DB_USER=<实际租户登录名>
DB_NAME=weknora
DB_PASSWORD=<安全注入>
AUTO_MIGRATE=false
RETRIEVE_DRIVER=elasticsearch_v8
ELASTICSEARCH_ADDR=<批准的ES8地址>
ELASTICSEARCH_INDEX=weknora_chunks_v1
REDIS_ADDR=<实际Redis地址>
STORAGE_TYPE=<local或批准的对象存储>
```

纯 Wiki 的知识库策略：`wiki_enabled=true`、`vector_enabled=false`、`keyword_enabled=false`、`graph_enabled=false`，指定可用的合成模型；后续启用文档检索时才配置所需 embedding。ES 账号／认证与 TLS 按实际部署配置，不在任务书保存真实凭证。

每个实现任务最后提交：修改文件列表、schema/接口变化、实际运行的测试与环境、失败与跳过项、剩余风险。**不得把“代码写好”“MySQL 测试通过”“SQLite 测试通过”写成“OceanBase 4.4.2 已验证”。**

## 4. 可直接交给后续智能体的启动提示词

> 请基于 reports 中的技术分析报告、实施任务书和代码依赖清单，适配当前 WeKnora 自动 Wiki 到 OceanBase 4.4.2 MySQL 模式，ES8 为派生检索层，保留 Redis 与文件存储。先核对实际 commit 和仓库指令，再执行 T01、T02 和 P0 任务。不要只改 DB_DRIVER；重点保障 grouped dedup_key claim、页面 CAS/历史事务、来源撤销、软删除唯一性与删除竞态。已有数据库凭证通过安全环境注入，缺少实例时继续可独立完成的代码设计并如实标记未实测。按验收门槛交付实现、测试结果和剩余风险；ES Wiki 索引、Outbox 与独立 index_generation 属后续明确改造，不冒充现成能力。
