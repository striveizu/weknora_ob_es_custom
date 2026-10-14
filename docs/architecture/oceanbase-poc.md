# OceanBase 4.4.2 + ES8 自动 Wiki 第一版 PoC

本版面向新安装、MySQL 兼容租户和自动 Wiki。OceanBase 保存业务数据；Redis 保留任务调度；ES8 复用文档分块检索适配器。应用代码由当前源码编译，数据库使用 `OceanBase_CE-v4.4.2.1`，ES 使用 8.19.6。

## 实现内容

- `DB_DRIVER=mysql` 接线，保留真实 mysql dialect；禁止把 PG 检索驱动绑定到该主库。
- 独立的嵌入式 baseline：72 张应用表及 `task_claim_lanes`，包含版本历史、目录、任务和权限数据；不执行 PostgreSQL 迁移或扩展。
- nullable generated marker 实现存活页面／目录等 partial unique 约束，允许删除后重建。
- JSON 原生存储、UTC DATETIME(6)、LONGTEXT 正文；JSON／大文本不声明不受支持的非空默认值。模型中声明的 JSON 默认值由方言作为参数写入。
- Wiki 列表、来源成员／前缀反查、路径过滤、归一化标题、字面搜索和 Go bigram 去重候选；保留页面 CAS＋历史快照事务。
- 按 task lane 的 DB 行锁串行化短认领／清空事务，同一文档的多条 op 一起认领；LLM 调用不持有 DB 事务。
- MySQL 分支的失败计数和 drain 不使用 RETURNING。模型引用查询、用户／组织搜索、存储绑定删除保护等补充 MySQL 分支。
- 迁移使用持久化 owner token，避免长期事务与 DDL 发布冲突；失败保留 dirty 状态，应用拒绝在错误 schema 上启动。

## 启动本机 PoC

`scripts/oceanbase_poc.py` 只控制 `weknora-ob-poc-*` 容器和同名网络／卷，不操作原有 WeKnora 服务。SQL、Redis 和 HTTP 端口仅绑定回环地址。建议 Docker VM 至少 12 GiB 内存，尤其已有其他服务时；低资源配置不作为生产参数。

```bash
python3 scripts/oceanbase_poc.py credentials
python3 scripts/oceanbase_poc.py infra
# 等待 OceanBase 完成 test 租户初始化。
python3 scripts/oceanbase_poc.py accounts
python3 scripts/oceanbase_poc.py migrate
python3 scripts/oceanbase_poc.py build
python3 scripts/oceanbase_poc.py app
```

启动脚本的 Go 镜像默认使用本机已有的 `weknora-app-builder:auto-register`（Go 1.26.8、CGO 编译环境）。其他机器需准备相同依赖的 Go 1.26 builder，并通过 `POC_GO_IMAGE` 指定；不能以普通镜像缺少 CGO／依赖工具的失败判定代码不可行。前端和 docreader 暂复用已有发布镜像，未修改其源码。

| 服务 | 本机地址 | Docker 网络地址 |
| --- | --- | --- |
| Wiki UI | `http://127.0.0.1:18081` | `weknora-ob-poc-ui:80` |
| 后端 | `http://127.0.0.1:18080` | `weknora-ob-poc-app:8080` |
| OceanBase | `127.0.0.1:12881` | `weknora-ob-poc-db:2881` |
| Elasticsearch | `http://127.0.0.1:18920` | `weknora-ob-poc-es:9200` |
| Redis | 不发布本机端口 | `weknora-ob-poc-redis:6379` |

数据库：`weknora_poc`；登录名：`weknora_poc@test`。密码和 JWT／Redis 配置保存在仓库根的 `.env.oceanbase-poc`，权限 0600，Git 忽略。`.env.oceanbase-poc-admin` 保存本地引导凭证；`.env.oceanbase-poc-tests` 保存只获准创建随机测试库的账号配置。不要把这些文件提交或放入容器镜像。

建一个知识库，关闭 vector／keyword／graph、打开 Wiki，并指定可用的合成模型。随后上传资料，观察生成状态、页面、来源和版本。启动系统与调用真实模型是不同验收项：未配置 LLM 时不能凭 UI 可访问宣称自动生成已经验证。

编译修改后，`build` 不会自动替换运行中的 app：停止并删除 **仅 PoC app 容器** 后重新执行 `app`，或以新的镜像版本部署。停止所有 PoC：

```bash
python3 scripts/oceanbase_poc.py stop
```

停止不删除数据卷或密码。`infra` 复用已有容器，不重置已有数据库。更改镜像、初始化密码或资源环境需显式安排容器重建；环境文件与已初始化数据库密码不能任意单独修改。

## 验证

真实数据库集成测试创建 `weknora_ob_test_<随机UUID>`，测试后只删除这个库，不清空 `weknora_poc`。

```bash
python3 scripts/oceanbase_poc.py test
```

覆盖：新库迁移与重复迁移、JSON 来源精确反查、超过 64 KiB 的正文、搜索与去重候选、同页并发 CAS／快照、软删除重建、按文档分组认领、晚到 sibling、并发失败计数、finalizing seed 与 drain。缺少测试 DSN 时会明确 skip，skip 不算 OceanBase 验证通过。

已有 Repository／数据库／类型测试、Wiki 服务测试和容器包编译检查应同时运行。具体实测结果以本次实现验证报告为准，不把 MySQL／SQLite 单测当成 OB 多 Worker 测试。

## 迁移失败与恢复

`oceanbase_schema_migrations` 记录 version／dirty；`oceanbase_migration_lock.owner` 记录排他 owner。初始化失败不自动 force-clean；进程崩溃可能留下 owner。恢复前确认没有活动 migrator，检查部分 DDL 和数据，然后由管理员显式修复。PoC 的随机空测试库可重新创建，但已有用户库不能自动删除重建。

baseline 由 `scripts/generate_oceanbase_baseline.py` 从 SQLite 当前 schema catalog 和 PG JSON 类型声明生成。`schema-manifest.json` 是列对照，源 commit 为 `94fae6de...`。已部署后修改 baseline 不能升级既有 version=1 的数据库；后续 schema 变化需新增有版本的升级路径。

## 本版边界

- Wiki 搜索仍在 OB；ES 的 Wiki 专用索引、Outbox、索引 generation、重建及权限回读尚未实现。
- 相似候选在 Go 中分页扫描轻量页面，适用于 PoC；大知识库应再接 ES 候选索引并评估召回。
- MySQL Repository 搜索采用字面匹配；不会将 ES regexp 或 MySQL regexp 冒充 PG 整段正文 POSIX 正则。Agent 原有 Go 搜索路径的合同另行验证。
- `chunk_images` 表存在，但 SQLite／PG 的素材投影触发器未移植；素材图库不在本版支持范围。图片原始字段不因此删除。
- 数据源、OAuth、所有可选工具、memory 大规模向量检索、完整共享与权限竞态还需完整回归。
- 保留既有约 90 分钟 stale claim 行为；owner fencing／租约续期与极端迟到旧 Worker 的强化属于后续任务，不能宣称当前协议提供 exactly-once。
- ES 文档向量仍使用原有 script_score，未实现 HNSW；现有分块写入 ID／跨库索引一致性未在此版全面重构。
- 不启用自建 Langfuse 的 PG 栈；没有把 Langfuse 一并改为 OceanBase。
- 本版无既有 PG 数据在线迁移、生产压测或高可用承诺。
