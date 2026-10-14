package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	obmigrations "github.com/Tencent/WeKnora/migrations/oceanbase"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// OB has JSON functions but not MySQL 8's expression defaults. Bind the
// model's declared JSON default when GORM would otherwise emit DEFAULT.
type oceanBaseDialector struct{ gorm.Dialector }

func (d oceanBaseDialector) Initialize(db *gorm.DB) error {
	if err := d.Dialector.Initialize(db); err != nil {
		return err
	}
	return db.Callback().Create().Before("gorm:create").Register("oceanbase:json_defaults", oceanBaseJSONDefaults)
}

// GORM omits zero-valued struct fields with a DB default, without calling
// DefaultValueOf. Clone only this statement's schema (never mutate the shared
// cached schema) and make tagged JSON defaults explicit client-side values.
func oceanBaseJSONDefaults(db *gorm.DB) {
	stmt := db.Statement
	if stmt.Schema == nil {
		return
	}
	changed := map[string]*schema.Field{}
	for _, field := range stmt.Schema.Fields {
		if (field.DataType != "json" && field.DataType != "jsonb") || !field.HasDefaultValue {
			continue
		}
		value := []byte(strings.Trim(field.DefaultValue, "'\""))
		if !json.Valid(value) {
			continue
		}
		fill := func(row reflect.Value) {
			for row.Kind() == reflect.Ptr {
				if row.IsNil() {
					return
				}
				row = row.Elem()
			}
			if row.Kind() != reflect.Struct {
				return
			}
			if _, zero := field.ValueOf(stmt.Context, row); zero {
				db.AddError(field.Set(stmt.Context, row, value))
			}
		}
		rv := stmt.ReflectValue
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			for i := 0; i < rv.Len(); i++ {
				fill(rv.Index(i))
			}
		} else {
			fill(rv)
		}
		copy := *field
		copy.HasDefaultValue = false
		copy.DefaultValueInterface = nil
		changed[field.Name] = &copy
	}
	if len(changed) == 0 {
		return
	}
	copy := *stmt.Schema
	copy.Fields = append([]*schema.Field(nil), stmt.Schema.Fields...)
	copy.FieldsByName = map[string]*schema.Field{}
	copy.FieldsByDBName = map[string]*schema.Field{}
	copy.FieldsByBindName = map[string]*schema.Field{}
	replace := func(field *schema.Field) *schema.Field {
		if f, ok := changed[field.Name]; ok {
			return f
		}
		return field
	}
	for i, field := range copy.Fields {
		copy.Fields[i] = replace(field)
	}
	for key, field := range stmt.Schema.FieldsByName {
		copy.FieldsByName[key] = replace(field)
	}
	for key, field := range stmt.Schema.FieldsByDBName {
		copy.FieldsByDBName[key] = replace(field)
	}
	for key, field := range stmt.Schema.FieldsByBindName {
		copy.FieldsByBindName[key] = replace(field)
	}
	copy.FieldsWithDefaultDBValue = nil
	for _, field := range stmt.Schema.FieldsWithDefaultDBValue {
		if _, ok := changed[field.Name]; !ok {
			copy.FieldsWithDefaultDBValue = append(copy.FieldsWithDefaultDBValue, field)
		}
	}
	stmt.Schema = &copy
}

func NewOceanBaseDialector(dsn string) gorm.Dialector {
	return oceanBaseDialector{gormmysql.New(gormmysql.Config{DSN: dsn, SkipInitializeWithVersion: true, DontSupportRenameIndex: true, DontSupportRenameColumn: true})}
}

func (d oceanBaseDialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if (field.DataType == "json" || field.DataType == "jsonb") && field.HasDefaultValue && field.DefaultValue != "" {
		return clause.Expr{SQL: "?", Vars: []interface{}{strings.Trim(field.DefaultValue, "'\"")}}
	}
	return d.Dialector.DefaultValueOf(field)
}

// MySQLDSN uses the driver formatter so tenant names and credentials are not
// interpolated into SQL. TLS is the driver's registered name or a standard
// mode (e.g. true); the local PoC can use false on a loopback-only endpoint.
func MySQLDSN() string {
	c := mysql.NewConfig()
	c.User, c.Passwd, c.DBName = os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_NAME")
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	c.ParseTime, c.Loc = true, time.UTC
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 10*time.Second, 45*time.Second, 30*time.Second
	c.TLSConfig = os.Getenv("DB_MYSQL_TLS")
	c.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'", "ob_query_timeout": "30000000"}
	return c.FormatDSN()
}

// RunOceanBaseMigrations uses an atomic durable owner token, not a transaction
// held across DDL (OB's schema publication can wait for open transactions).
// A crashed migrator leaves the token and/or dirty flag for explicit repair.
// Neither is auto-expired: an old worker must never resume concurrent DDL.
func RunOceanBaseMigrations(ctx context.Context, dsn string) error {
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("invalid MySQL connection configuration")
	}
	c.MultiStatements = true
	c.ReadTimeout = 5 * time.Minute
	db, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(3)
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS oceanbase_migration_lock (id INT PRIMARY KEY, owner VARCHAR(64) NOT NULL DEFAULT '') DEFAULT CHARSET=utf8mb4`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO oceanbase_migration_lock (id) VALUES (1) ON DUPLICATE KEY UPDATE id=id`); err != nil {
		return err
	}
	owner := uuid.NewString()
	result, err := db.ExecContext(ctx, `UPDATE oceanbase_migration_lock SET owner=? WHERE id=1 AND owner=''`, owner)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("OceanBase migration is owned by another process or an abandoned owner; verify no migrator is active before manual repair")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.ExecContext(releaseCtx, `UPDATE oceanbase_migration_lock SET owner='' WHERE id=1 AND owner=?`, owner)
	}()
	if _, err := db.ExecContext(ctx, `SET ob_query_timeout=300000000`); err != nil {
		return fmt.Errorf("set OceanBase migration query timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS oceanbase_schema_migrations (id INT PRIMARY KEY, version INT NOT NULL, dirty BOOLEAN NOT NULL)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO oceanbase_schema_migrations (id,version,dirty) VALUES (1,0,false) ON DUPLICATE KEY UPDATE id=id`); err != nil {
		return err
	}
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version,dirty FROM oceanbase_schema_migrations WHERE id=1`).Scan(&version, &dirty); err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("OceanBase schema is dirty at version %d: repair partial DDL explicitly; automatic recovery is disabled", version)
	}
	if version == obmigrations.Version {
		setMigrationState(uint(version), false, "", true)
		return nil
	}
	if version != 0 {
		return fmt.Errorf("unsupported OceanBase schema version %d (expected %d)", version, obmigrations.Version)
	}
	if _, err := db.ExecContext(ctx, `UPDATE oceanbase_schema_migrations SET version=?,dirty=true WHERE id=1`, obmigrations.Version); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, obmigrations.Baseline); err != nil {
		setMigrationState(obmigrations.Version, true, err.Error(), true)
		return fmt.Errorf("OceanBase baseline failed (schema remains dirty): %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE oceanbase_schema_migrations SET dirty=false WHERE id=1`); err != nil {
		return err
	}
	setMigrationState(obmigrations.Version, false, "", true)
	return nil
}

func ValidateOceanBaseSchema(ctx context.Context, db *sql.DB) error {
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version,dirty FROM oceanbase_schema_migrations WHERE id=1`).Scan(&version, &dirty); err != nil {
		return fmt.Errorf("OceanBase schema has not been initialized: %w", err)
	}
	if version != obmigrations.Version || dirty {
		return fmt.Errorf("OceanBase schema mismatch: version=%d dirty=%v expected=%d", version, dirty, obmigrations.Version)
	}
	setMigrationState(uint(version), false, "", true)
	return nil
}
