# migrations/mysql：历史一次性建表脚本

这里的 `00-init-db.sql` 仍不会被运行时执行，只有 10 张核心表，不能用于安装当前完整应用。

本分支新增 `DB_DRIVER=mysql`，目标是 **OceanBase 4.4.2 MySQL 模式、Wiki 优先的新安装 PoC**。它使用 `migrations/oceanbase/` 的嵌入式 baseline 和独立迁移命令，不使用本目录，也不执行 PostgreSQL 历史迁移。

| DB_DRIVER | 迁移来源 |
| --- | --- |
| postgres | migrations/versioned/ |
| sqlite | migrations/sqlite/ |
| mysql | migrations/oceanbase/，通过 cmd/oceanbase-migrate 执行 |

部署、实测、迁移失败处理和已知边界见 [OceanBase PoC 说明](../../docs/architecture/oceanbase-poc.md)。当前不承诺所有 MySQL 发行版、全部可选模块或既有数据库在线升级已验证。
