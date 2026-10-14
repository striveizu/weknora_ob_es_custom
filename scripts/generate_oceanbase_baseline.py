#!/usr/bin/env python3
"""Generate the fresh-install OB MySQL baseline from the current SQLite schema.

SQLite migrations already maintain the application schema without PG extensions.
Apply all of them, then translate the resulting catalog explicitly. This does not
migrate existing data. SQLite image projection triggers are deliberately excluded
and the PoC documentation identifies the image gallery as unsupported.
"""
import json
import re
import sqlite3
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "migrations/oceanbase"


def quote(name):
    return "`" + name.replace("`", "``") + "`"


def generate():
    db = sqlite3.connect(":memory:")
    paths = sorted((ROOT / "migrations/sqlite").glob("*.up.sql"))
    for path in paths:
        db.executescript(path.read_text())
    tables = db.execute("SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name").fetchall()
    json_columns = set()
    for path in (ROOT / "migrations/versioned").glob("*.up.sql"):
        sql = re.sub(r"--[^\n]*", "", path.read_text())
        for table, body in re.findall(r"CREATE TABLE(?: IF NOT EXISTS)?\s+(\w+)\s*\((.*?)\);", sql, re.S | re.I):
            for col in re.findall(r"(?:^|\n)\s*(\w+)\s+JSONB\b", body, re.I):
                json_columns.add((table, col))
        for table, col in re.findall(r"ALTER TABLE\s+(\w+)\s+ADD COLUMN(?: IF NOT EXISTS)?\s+(\w+)\s+JSONB\b", sql, re.I):
            json_columns.add((table, col))
    statements = []
    manifest = {"source_commit": "94fae6de43a36e44d64aecbbdf19c0b72b352aa8", "sqlite_version": 36, "schema_version": 1, "tables": {}}
    foreign_keys = []
    for table, sql in tables:
        columns = db.execute("PRAGMA table_info(" + quote(table) + ")").fetchall()
        definitions = []
        types = {}
        for _, name, dtype, required, default, pk in columns:
            if dtype == "INTEGER":
                dtype = "BIGINT"
            if dtype == "DATETIME":
                dtype = "DATETIME(6)"
                if default == "CURRENT_TIMESTAMP":
                    default = "CURRENT_TIMESTAMP(6)"
            if dtype == "BLOB":
                dtype = "LONGBLOB"
            if dtype == "TEXT":
                dtype = "JSON" if (table, name) in json_columns or default in ("'[]'", "'{}'") else "LONGTEXT"
            # OB's MySQL mode rejects non-NULL JSON/TEXT defaults. The GORM
            # dialect supplies tagged JSON defaults as bound values instead.
            if dtype in ("JSON", "LONGTEXT") and default != "NULL":
                default = None
            # These are indexed strings in the relational schema, not JSON.
            if (table, name) in {("chunk_images", "image_key"), ("message_artifacts", "url")}:
                dtype = "VARCHAR(2048)"
            if table == "tenant_api_keys" and name == "key_hash":
                dtype = "VARCHAR(64)"
            if table == "fork_snapshot_leases" and name == "snapshot_id":
                dtype = "VARCHAR(128)"
            if table == "fork_snapshot_leases" and name == "sandbox_config_id":
                dtype = "VARCHAR(36)"
            if pk and dtype in ("JSON", "LONGTEXT", "LONGBLOB"):
                raise ValueError("Primary key requires an explicit bounded type: " + table + "." + name)
            auto = (name == "id" and pk and dtype == "BIGINT" and table != "model_catalog_configs") or (table in ("chunks", "knowledge_tags") and name == "seq_id")
            if auto:
                dtype, required, default = "BIGINT", 1, None
            definition = quote(name) + " " + dtype
            if required or pk:
                definition += " NOT NULL"
            if default is not None:
                definition += " DEFAULT " + default
            if auto:
                definition += " AUTO_INCREMENT"
            definitions.append(definition)
            types[name] = dtype
        pk = [quote(c[1]) for c in sorted(columns, key=lambda c: c[5]) if c[5]]
        if pk:
            definitions.append("PRIMARY KEY (" + ", ".join(pk) + ")")
        # Include SQLite autoindexes (inline UNIQUE constraints), as well as
        # explicit indexes. Recreate partial uniqueness with nullable markers.
        for _, idx, unique, origin, partial in db.execute("PRAGMA index_list(" + quote(table) + ")").fetchall():
            if origin == "pk":
                continue
            indexed = [r[2] for r in db.execute("PRAGMA index_info(" + quote(idx) + ")")]
            if any(c is None for c in indexed):
                raise ValueError("Expression index needs explicit translation: " + idx)
            idx_sql = db.execute("SELECT sql FROM sqlite_master WHERE name=?", (idx,)).fetchone()[0]
            idx_name = idx if origin != "u" else "uq_" + table + "_" + "_".join(indexed)
            marker = None
            if partial and unique:
                predicate = re.split(r"\bWHERE\b", idx_sql, flags=re.I)[1].strip()
                marker = "live_" + str(len(definitions))
                definitions.append(quote(marker) + " TINYINT GENERATED ALWAYS AS (CASE WHEN " + predicate + " THEN 1 ELSE NULL END) STORED")
            expressions = []
            for col in indexed:
                expr = quote(col)
                # Non-unique text search indexes may use prefixes. Never
                # truncate a business unique key.
                if types[col] in ("LONGTEXT", "JSON"):
                    if unique:
                        raise ValueError("Unique text index requires explicit full key: " + idx)
                    expr += "(191)"
                if table == "wiki_pages" and col == "wiki_path":
                    expr += "(191)"
                if table == "message_artifacts" and col == "url":
                    expr += "(191)"
                expressions.append(expr)
            if marker:
                expressions.append(quote(marker))
            definitions.append(("UNIQUE KEY " if unique else "KEY ") + quote(idx_name[:64]) + " (" + ", ".join(expressions) + ")")
        if table in ("chunks", "knowledge_tags"):
            definitions.append("UNIQUE KEY " + quote("uq_" + table + "_seq_id") + " (`seq_id`)")
        statements.append("CREATE TABLE " + quote(table) + " (\n    " + ",\n    ".join(definitions) + "\n) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;")
        groups = {}
        for fid, seq, parent, col, target, update, delete, _ in db.execute("PRAGMA foreign_key_list(" + quote(table) + ")"):
            groups.setdefault(fid, []).append((seq, parent, col, target, update, delete))
        for fid, parts in groups.items():
            parts.sort()
            _, parent, _, _, update, delete = parts[0]
            foreign_keys.append("ALTER TABLE " + quote(table) + " ADD CONSTRAINT " + quote("fk_" + table + "_" + str(fid)) + " FOREIGN KEY (" + ", ".join(quote(p[2]) for p in parts) + ") REFERENCES " + quote(parent) + " (" + ", ".join(quote(p[3]) for p in parts) + ") ON UPDATE " + update + " ON DELETE " + delete + ";")
        manifest["tables"][table] = types
    statements.extend(foreign_keys)
    statements.append("INSERT INTO model_catalog_configs (id, version, overlay, history) VALUES (1, 0, '{\"providers\":{}}', '[]');")
    # A durable, per-lane mutex serializes only the short claim/cleanup
    # transactions. LLM work runs outside these transactions.
    statements.append("CREATE TABLE task_claim_lanes (task_type VARCHAR(64) NOT NULL, scope VARCHAR(32) NOT NULL, scope_id VARCHAR(64) NOT NULL, PRIMARY KEY(task_type,scope,scope_id)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;")
    OUT.mkdir(exist_ok=True)
    (OUT / "000001_baseline.up.sql").write_text("-- Fresh-install PoC baseline. Generated by scripts/generate_oceanbase_baseline.py.\n-- Do not apply to an existing PostgreSQL/MySQL schema. No image projection triggers.\n\n" + "\n\n".join(statements) + "\n")
    (OUT / "schema-manifest.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
    print("Generated", len(tables), "application tables plus task_claim_lanes")


if __name__ == "__main__":
    generate()
