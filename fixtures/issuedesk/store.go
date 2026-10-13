package issuedesk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }

const ticketColumns = `id,title,body,status,creator_id,assignee_id,version,created_at,updated_at`

func scanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.Title, &t.Body, &t.Status, &t.CreatorID, &t.AssigneeID, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}
func (s Store) actor(ctx context.Context, token string) (Actor, error) {
	var a Actor
	err := s.Pool.QueryRow(ctx, `SELECT id,tenant_id,name,role FROM users WHERE token_hash=$1`, tokenHash(token)).Scan(&a.ID, &a.TenantID, &a.Name, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, fail(http.StatusUnauthorized, "unauthorized", "身份凭证无效")
	}
	return a, err
}
func visible(a Actor, t Ticket) bool { return a.Role != "requester" || t.CreatorID == a.ID }
func (s Store) get(ctx context.Context, a Actor, id int64) (Ticket, error) {
	t, err := scanTicket(s.Pool.QueryRow(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL`, a.TenantID, id))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !visible(a, t) {
		return t, fail(404, "not_found", "工单不存在")
	}
	return t, err
}

type createInput struct {
	Title          string `json:"title"`
	Body           string `json:"body"`
	InitialComment string `json:"initial_comment,omitempty"`
}

func validText(s string, min, max int) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	return n >= min && utf8.RuneCountInString(s) <= max
}
func (s Store) create(ctx context.Context, a Actor, key string, in createInput) (Ticket, bool, error) {
	var out Ticket
	if len(key) < 1 || len(key) > 128 {
		return out, false, fail(400, "invalid_idempotency_key", "创建工单需要 1 至 128 字节的 Idempotency-Key")
	}
	if !validText(in.Title, 1, 200) || !validText(in.Body, 0, 20000) || !validText(in.InitialComment, 0, 10000) {
		return out, false, fail(400, "invalid_input", "标题或正文长度无效")
	}
	requestHash := tokenHash(string(jsonBytes(in)))
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)
	lock := fmt.Sprintf("issuedesk:%d:%d:%s", a.TenantID, a.ID, key)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return out, false, err
	}
	var savedHash string
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM idempotency_keys WHERE tenant_id=$1 AND actor_id=$2 AND key=$3`, a.TenantID, a.ID, key).Scan(&savedHash, &saved)
	if err == nil {
		if savedHash != requestHash {
			return out, false, fail(409, "idempotency_conflict", "相同幂等键对应不同请求")
		}
		if err = json.Unmarshal(saved, &out); err != nil {
			return out, false, err
		}
		return out, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	out, err = scanTicket(tx.QueryRow(ctx, `INSERT INTO tickets(tenant_id,title,body,creator_id) VALUES($1,$2,$3,$4) RETURNING `+ticketColumns, a.TenantID, strings.TrimSpace(in.Title), in.Body, a.ID))
	if err != nil {
		return out, false, err
	}
	if in.InitialComment != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO comments(tenant_id,ticket_id,author_id,body) VALUES($1,$2,$3,$4)`, a.TenantID, out.ID, a.ID, in.InitialComment); err != nil {
			return out, false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO idempotency_keys(tenant_id,actor_id,key,request_hash,status,response) VALUES($1,$2,$3,$4,201,$5)`, a.TenantID, a.ID, key, requestHash, jsonBytes(out)); err != nil {
		return out, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Ticket{}, false, err
	}
	return out, false, nil
}

type cursor struct {
	Created time.Time `json:"created"`
	ID      int64     `json:"id"`
	Filter  string    `json:"filter"`
}
type listInput struct {
	Status, Q, Assignee, Cursor string
	Limit                       int
}
type ticketPage struct {
	Items      []Ticket `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
func (s Store) list(ctx context.Context, a Actor, in listInput) (ticketPage, error) {
	out := ticketPage{Items: []Ticket{}}
	if in.Limit < 1 || in.Limit > 100 {
		return out, fail(400, "invalid_limit", "limit 必须在 1 至 100 之间")
	}
	statuses := map[string]bool{"": true, "open": true, "in_progress": true, "resolved": true, "closed": true, "cancelled": true}
	if !statuses[in.Status] {
		return out, fail(400, "invalid_status", "状态过滤条件无效")
	}
	var assignee int64
	var err error
	if in.Assignee != "" {
		assignee, err = strconv.ParseInt(in.Assignee, 10, 64)
		if err != nil || assignee < 1 {
			return out, fail(400, "invalid_assignee", "分派人员无效")
		}
	}
	fingerprint := tokenHash(fmt.Sprintf("%d:%d:%s:%s:%s:%s", a.TenantID, a.ID, a.Role, in.Status, in.Q, in.Assignee))
	var c cursor
	if in.Cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(in.Cursor)
		if e != nil || json.Unmarshal(b, &c) != nil || c.ID < 1 || c.Created.IsZero() || c.Filter != fingerprint {
			return out, fail(400, "invalid_cursor", "分页游标与当前查询不匹配")
		}
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE tenant_id=$1 AND deleted_at IS NULL
 AND ($2 <> 'requester' OR creator_id=$3) AND ($4='' OR status=$4)
 AND ($5='' OR title ILIKE $6 ESCAPE '\' OR body ILIKE $6 ESCAPE '\')
 AND ($7::bigint=0 OR assignee_id=$7) AND ($8::bigint=0 OR (created_at,id)<($9::timestamptz,$8))
 ORDER BY created_at DESC,id DESC LIMIT $10`, a.TenantID, a.Role, a.ID, in.Status, in.Q, "%"+escapeLike(in.Q)+"%", assignee, c.ID, c.Created, in.Limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		t, e := scanTicket(rows)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, t)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > in.Limit {
		out.Items = out.Items[:in.Limit]
		last := out.Items[len(out.Items)-1]
		out.NextCursor = base64.RawURLEncoding.EncodeToString(jsonBytes(cursor{last.CreatedAt, last.ID, fingerprint}))
	}
	return out, nil
}

type updateInput struct {
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Status     *string `json:"status,omitempty"`
	AssigneeID *int64  `json:"assignee_id,omitempty"`
	Unassign   bool    `json:"unassign,omitempty"`
	Comment    *string `json:"comment,omitempty"`
}

func (s Store) update(ctx context.Context, a Actor, id, version int64, in updateInput) (Ticket, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback(ctx)
	current, err := scanTicket(tx.QueryRow(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, a.TenantID, id))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !visible(a, current) {
		return Ticket{}, fail(404, "not_found", "工单不存在")
	}
	if err != nil {
		return Ticket{}, err
	}
	if current.Version != version {
		return Ticket{}, fail(409, "version_conflict", "工单已更新，请刷新后重试")
	}
	if in.Title == nil && in.Body == nil && in.Status == nil && in.AssigneeID == nil && !in.Unassign && in.Comment == nil {
		return Ticket{}, fail(400, "empty_update", "至少需要一个更新字段")
	}
	if in.Title != nil {
		if !validText(*in.Title, 1, 200) {
			return Ticket{}, fail(400, "invalid_input", "标题长度无效")
		}
		current.Title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		if !validText(*in.Body, 0, 20000) {
			return Ticket{}, fail(400, "invalid_input", "正文长度无效")
		}
		current.Body = *in.Body
	}
	if in.Status != nil {
		if !allowedTransition(a.Role, current.Status, *in.Status) {
			return Ticket{}, fail(409, "invalid_transition", "当前角色不能执行此状态流转")
		}
		current.Status = *in.Status
	}
	if in.AssigneeID != nil && in.Unassign {
		return Ticket{}, fail(400, "invalid_assignee", "不能同时分派与取消分派")
	}
	if in.AssigneeID != nil || in.Unassign {
		if a.Role == "requester" {
			return Ticket{}, fail(403, "forbidden", "只有客服或管理员可以分派工单")
		}
		if in.Unassign {
			current.AssigneeID = nil
		} else {
			var valid bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE tenant_id=$1 AND id=$2 AND role IN ('agent','admin'))`, a.TenantID, *in.AssigneeID).Scan(&valid)
			if err != nil {
				return Ticket{}, err
			}
			if !valid {
				return Ticket{}, fail(400, "invalid_assignee", "分派人员必须是本租户客服或管理员")
			}
			current.AssigneeID = in.AssigneeID
		}
	}
	if in.Comment != nil && !validText(*in.Comment, 1, 10000) {
		return Ticket{}, fail(400, "invalid_input", "评论长度无效")
	}
	updated, err := scanTicket(tx.QueryRow(ctx, `UPDATE tickets SET title=$1,body=$2,status=$3,assignee_id=$4,version=version+1,updated_at=clock_timestamp() WHERE tenant_id=$5 AND id=$6 AND version=$7 AND deleted_at IS NULL RETURNING `+ticketColumns, current.Title, current.Body, current.Status, current.AssigneeID, a.TenantID, id, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, fail(409, "version_conflict", "工单已更新，请刷新后重试")
	}
	if err != nil {
		return Ticket{}, err
	}
	if in.Comment != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO comments(tenant_id,ticket_id,author_id,body) VALUES($1,$2,$3,$4)`, a.TenantID, id, a.ID, *in.Comment); err != nil {
			return Ticket{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Ticket{}, err
	}
	return updated, nil
}
func (s Store) comments(ctx context.Context, a Actor, id int64) ([]Comment, error) {
	if _, err := s.get(ctx, a, id); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT id,author_id,body,created_at FROM comments WHERE tenant_id=$1 AND ticket_id=$2 ORDER BY id`, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err = rows.Scan(&c.ID, &c.AuthorID, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s Store) remove(ctx context.Context, a Actor, id, version int64) error {
	if a.Role != "admin" {
		return fail(403, "forbidden", "只有管理员可以删除工单")
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE tickets SET deleted_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND version=$3 AND deleted_at IS NULL`, a.TenantID, id, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err = s.get(ctx, a, id); err != nil {
			return err
		}
		return fail(409, "version_conflict", "工单已更新，请刷新后重试")
	}
	return nil
}
