package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/database"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This suite creates and drops only a randomly named, suite-owned database.
// The opt-in DSN must have CREATE DATABASE permission on an isolated test tenant.
func TestOceanBaseWikiIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := os.Getenv("WEKNORA_OCEANBASE_TEST_DSN")
	if dsn == "" {
		t.Skip("WEKNORA_OCEANBASE_TEST_DSN is required for real OceanBase integration")
	}
	c, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	c.DBName = ""
	c.ParseTime = true
	c.Loc = time.UTC
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	c.Params["ob_query_timeout"] = "30000000"
	admin, err := sql.Open("mysql", c.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var version string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	require.Contains(t, strings.ToLower(version), "oceanbase")
	t.Log("Database version:", version)
	name := "weknora_ob_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
	require.NoError(t, err)
	t.Cleanup(func() {
		dropCtx, done := context.WithTimeout(context.Background(), 3*time.Minute)
		defer done()
		_, e := admin.ExecContext(dropCtx, "DROP DATABASE `"+name+"`")
		require.NoError(t, e)
	})
	c.DBName = name
	require.NoError(t, database.RunOceanBaseMigrations(ctx, c.FormatDSN()))
	require.NoError(t, database.RunOceanBaseMigrations(ctx, c.FormatDSN()))
	db, err := gorm.Open(database.NewOceanBaseDialector(c.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, database.ValidateOceanBaseSchema(ctx, sqlDB))
	require.NoError(t, db.Exec("INSERT INTO tenants(id,name,business,retriever_engines) VALUES(10001,'PoC','test','[]')").Error)
	require.NoError(t, db.Exec("INSERT INTO knowledge_bases(id,tenant_id,name,embedding_model_id,summary_model_id,chunking_config,image_processing_config,cos_config,vlm_config) VALUES('kb-test',10001,'Wiki','','','{}','{}','{}','{}')").Error)
	pages := NewWikiPageRepository(db)
	t.Run("UserDefaultsAndDatabaseAssignedSequences", func(t *testing.T) {
		user := &types.User{ID: uuid.NewString(), Username: "poc-defaults", Email: "poc-defaults@example.invalid", PasswordHash: "test-only"}
		require.NoError(t, db.Create(user).Error)
		var prefs string
		require.NoError(t, db.Raw("SELECT CAST(preferences AS CHAR) FROM users WHERE id=?", user.ID).Scan(&prefs).Error)
		require.JSONEq(t, "{}", prefs)
		chunk := &types.Chunk{ID: uuid.NewString(), TenantID: 10001, KnowledgeBaseID: "kb-test", KnowledgeID: "doc-seq", Content: "证据分块", ChunkType: types.ChunkTypeText, IsEnabled: true}
		require.NoError(t, NewChunkRepository(db).CreateChunks(ctx, []*types.Chunk{chunk}))
		var seq int64
		require.NoError(t, db.Raw("SELECT seq_id FROM chunks WHERE id=?", chunk.ID).Scan(&seq).Error)
		require.Positive(t, seq)
		require.Equal(t, seq, chunk.SeqID, "new chunk sequence must be returned to the caller")
		tag := &types.KnowledgeTag{ID: uuid.NewString(), TenantID: 10001, KnowledgeBaseID: "kb-test", Name: "PoC tag"}
		require.NoError(t, NewKnowledgeTagRepository(db).Create(ctx, tag))
		require.Positive(t, tag.SeqID, "new tag sequence must be returned to the caller")
	})
	page := &types.WikiPage{ID: uuid.NewString(), TenantID: 10001, KnowledgeBaseID: "kb-test", Slug: "entity/oceanbase", Title: "OceanBase 数据库", PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusPublished, Content: strings.Repeat("中文 wiki。", 15000), Summary: "数据库", Version: 1, SourceRefs: types.StringArray{"doc%_|标题", "plain-doc"}, CategoryPath: types.StringArray{"数据库"}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, pages.Create(ctx, page))
	t.Run("JSONSourcesLongTextAndSearch", func(t *testing.T) {
		got, e := pages.GetBySlug(ctx, "kb-test", page.Slug)
		require.NoError(t, e)
		require.Equal(t, page.Content, got.Content)
		for _, id := range []string{"doc%_", "plain-doc"} {
			refs, e := pages.ListBySourceRef(ctx, "kb-test", id)
			require.NoError(t, e)
			require.Len(t, refs, 1)
		}
		refs, e := pages.ListBySourceRef(ctx, "kb-test", "doc")
		require.NoError(t, e)
		require.Empty(t, refs)
		slugs, e := pages.ListSlugsBySourceRef(ctx, "kb-test", "plain-doc")
		require.NoError(t, e)
		require.Equal(t, []string{page.Slug}, slugs)
		rows, e := pages.SearchAcross(ctx, []string{"kb-test"}, "oceanbase", 10)
		require.NoError(t, e)
		require.Len(t, rows, 1)
		_, count, e := pages.List(ctx, &types.WikiPageListRequest{KnowledgeBaseID: "kb-test", Query: "OCEANBASE", CategoryPath: []string{"数据库"}, SortBy: "wiki_path"})
		require.NoError(t, e)
		require.EqualValues(t, 1, count)
		similar, e := pages.FindSimilarPages(ctx, "kb-test", "OceanBase数据库", nil, 10)
		require.NoError(t, e)
		require.Len(t, similar, 1)
		normalized, e := pages.FindPagesByNormalizedTitle(ctx, "kb-test", types.WikiPageTypeEntity, "oceanbase数据库")
		require.NoError(t, e)
		require.Len(t, normalized, 1)
		orphans, e := pages.CountOrphans(ctx, "kb-test")
		require.NoError(t, e)
		require.EqualValues(t, 1, orphans)
	})
	t.Run("ConcurrentVersionSnapshotAndDeleteRecreate", func(t *testing.T) {
		a := *page
		b := *page
		a.Content = "first"
		b.Content = "second"
		errs := make(chan error, 2)
		for _, p := range []*types.WikiPage{&a, &b} {
			go func(p *types.WikiPage) {
				errs <- pages.UpdateWithRevision(ctx, p, &types.WikiPageRevision{ID: uuid.NewString(), TenantID: 10001, KnowledgeBaseID: "kb-test", PageID: page.ID, Slug: page.Slug, Version: 1, Title: page.Title, Content: page.Content, EditedAt: page.UpdatedAt, CreatedAt: time.Now().UTC()})
			}(p)
		}
		e1, e2 := <-errs, <-errs
		require.True(t, (e1 == nil) != (e2 == nil))
		if e1 != nil {
			require.ErrorIs(t, e1, ErrWikiPageConflict)
		}
		if e2 != nil {
			require.ErrorIs(t, e2, ErrWikiPageConflict)
		}
		_, count, e := pages.ListRevisions(ctx, "kb-test", page.ID, 10, 0)
		require.NoError(t, e)
		require.EqualValues(t, 1, count)
		duplicate := *page
		duplicate.ID = uuid.NewString()
		require.Error(t, pages.Create(ctx, &duplicate))
		require.NoError(t, pages.Delete(ctx, "kb-test", page.Slug))
		require.NoError(t, pages.Create(ctx, &duplicate))
	})
	t.Run("GroupedClaimsLateSiblingAndFailureCounters", func(t *testing.T) {
		queue := &taskPendingOpsRepository{db: db}
		for _, key := range []string{"a", "b"} {
			for _, op := range []string{"ingest", "retract"} {
				require.NoError(t, queue.Enqueue(ctx, &types.TaskPendingOp{TenantID: 10001, TaskType: "wiki:ingest", Scope: "test", ScopeID: "claims", Op: op, DedupKey: key}))
			}
		}
		results := make(chan []*types.TaskPendingOp, 2)
		failures := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				rows, e := queue.ClaimBatch(ctx, "wiki:ingest", "test", "claims", 1, time.Now().Add(-time.Hour))
				results <- rows
				failures <- e
			}()
		}
		first, second := <-results, <-results
		require.NoError(t, <-failures)
		require.NoError(t, <-failures)
		require.Len(t, first, 2)
		require.Len(t, second, 2)
		require.NotEqual(t, first[0].DedupKey, second[0].DedupKey)
		late := &types.TaskPendingOp{TenantID: 10001, TaskType: "wiki:ingest", Scope: "test", ScopeID: "claims", Op: "retract", DedupKey: first[0].DedupKey}
		require.NoError(t, queue.Enqueue(ctx, late))
		rows, e := queue.ClaimBatch(ctx, "wiki:ingest", "test", "claims", 10, time.Now().Add(-time.Hour))
		require.NoError(t, e)
		require.Empty(t, rows)
		var wg sync.WaitGroup
		counts := make(chan int, 8)
		fail := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); n, e := queue.IncrFailCount(ctx, late.ID); counts <- n; fail <- e }()
		}
		wg.Wait()
		close(counts)
		close(fail)
		seen := map[int]bool{}
		for n := range counts {
			seen[n] = true
		}
		for e := range fail {
			require.NoError(t, e)
		}
		require.Len(t, seen, 8)
	})
	t.Run("GuardedSeedAndDrain", func(t *testing.T) {
		queue := &taskPendingOpsRepository{db: db}
		require.NoError(t, db.Exec("INSERT INTO knowledges(id,tenant_id,knowledge_base_id,type,title,source,parse_status,custom_metadata) VALUES('doc-drain',10001,'kb-test','file','doc','','processing','{}')").Error)
		op := &types.TaskPendingOp{TenantID: 10001, TaskType: "wiki:ingest", Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-test", Op: "ingest", DedupKey: "doc-drain"}
		accepted, e := queue.SeedKnowledgeFinalizingWithPendingOp(ctx, "doc-drain", 1, op)
		require.NoError(t, e)
		require.True(t, accepted)
		keys, e := queue.DrainUnclaimedAndRelease(ctx, "wiki:ingest", types.TaskScopeKnowledgeBase, "kb-test", "ingest", time.Now().Add(-time.Hour))
		require.NoError(t, e)
		require.Equal(t, []string{"doc-drain"}, keys)
		var status string
		require.NoError(t, db.Raw("SELECT parse_status FROM knowledges WHERE id='doc-drain'").Scan(&status).Error)
		require.Equal(t, "completed", status)
	})
	t.Run("FolderDeleteRecreateAndMigrationGuards", func(t *testing.T) {
		folder := &types.WikiFolder{ID: uuid.NewString(), TenantID: 10001, KnowledgeBaseID: "kb-test", Name: "数据库", Path: "数据库", Depth: 1}
		require.NoError(t, pages.CreateFolder(ctx, folder))
		duplicate := *folder
		duplicate.ID = uuid.NewString()
		require.Error(t, pages.CreateFolder(ctx, &duplicate))
		require.NoError(t, pages.DeleteFolder(ctx, "kb-test", folder.ID))
		require.NoError(t, pages.CreateFolder(ctx, &duplicate))
		require.NoError(t, db.Exec("UPDATE oceanbase_schema_migrations SET dirty=true WHERE id=1").Error)
		require.Error(t, database.ValidateOceanBaseSchema(ctx, sqlDB))
		require.ErrorContains(t, database.RunOceanBaseMigrations(ctx, c.FormatDSN()), "dirty")
		require.NoError(t, db.Exec("UPDATE oceanbase_schema_migrations SET dirty=false WHERE id=1").Error)
		require.NoError(t, db.Exec("UPDATE oceanbase_migration_lock SET owner='abandoned-test-owner' WHERE id=1").Error)
		require.ErrorContains(t, database.RunOceanBaseMigrations(ctx, c.FormatDSN()), "owned")
		require.NoError(t, db.Exec("UPDATE oceanbase_migration_lock SET owner='' WHERE id=1").Error)
	})
}
