package issuedesk

import (
	"context"
	"embed"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sort"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate 按顺序执行事务迁移，校验已应用脚本，允许并发启动和重复执行。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(781326910)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, f := range files {
		body, err := migrations.ReadFile("migrations/" + f.Name())
		if err != nil {
			return err
		}
		checksum := tokenHash(string(body))
		var old string
		err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE name=$1`, f.Name()).Scan(&old)
		if err == nil {
			if old != checksum {
				return fmt.Errorf("迁移校验失败：%s", f.Name())
			}
			continue
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("迁移 %s：%w", f.Name(), err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)`, f.Name(), checksum); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Seed 只生成显式的合成演示数据，重复运行不会替换用户令牌或现有工单。
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO tenants(id,name) VALUES(1,'示例甲公司'),(2,'示例乙公司') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	for _, u := range []struct {
		id, tenant        int64
		name, role, token string
	}{{101, 1, "甲客户", "requester", "demo-alpha-requester"}, {102, 1, "甲客服", "agent", "demo-alpha-agent"}, {103, 1, "甲管理员", "admin", "demo-alpha-admin"}, {201, 2, "乙客户", "requester", "demo-beta-requester"}, {202, 2, "乙客服", "agent", "demo-beta-agent"}, {203, 2, "乙管理员", "admin", "demo-beta-admin"}} {
		if _, err = tx.Exec(ctx, `INSERT INTO users(id,tenant_id,name,role,token_hash) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, u.id, u.tenant, u.name, u.role, tokenHash(u.token)); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, demo := range []struct {
		actor Actor
		title string
	}{
		{Actor{ID: 101, TenantID: 1, Role: "requester"}, "示例：打印机无法连接"},
		{Actor{ID: 201, TenantID: 2, Role: "requester"}, "示例：会议室设备巡检"},
	} {
		if _, _, err = (Store{pool}).create(ctx, demo.actor, "synthetic-seed-v1", createInput{Title: demo.title, Body: "此工单为合成演示数据，不包含真实客户信息。", InitialComment: "已收到示例请求，等待客服处理。"}); err != nil {
			return err
		}
	}
	return nil
}
