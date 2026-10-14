package database

import (
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMySQLDSNPreservesTenantCredentials(t *testing.T) {
	t.Setenv("DB_HOST", "::1")
	t.Setenv("DB_PORT", "2881")
	t.Setenv("DB_USER", "app@tenant#cluster")
	t.Setenv("DB_PASSWORD", "test-only:!@#/+?=&%")
	t.Setenv("DB_NAME", "weknora")
	t.Setenv("DB_MYSQL_TLS", "false")
	c, err := mysql.ParseDSN(MySQLDSN())
	require.NoError(t, err)
	require.Equal(t, "app@tenant#cluster", c.User)
	require.Equal(t, "test-only:!@#/+?=&%", c.Passwd)
	require.Equal(t, "[::1]:2881", c.Addr)
	require.True(t, c.ParseTime)
	require.False(t, c.MultiStatements)
}
