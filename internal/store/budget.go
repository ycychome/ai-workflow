package store

import (
	"context"
	"errors"
	"time"
)

var ErrDailyBudget = errors.New("daily model request budget exhausted")
var ErrIssueBudget = errors.New("issue model request budget exhausted")

func (s *Store) DailyRequestRemaining(ctx context.Context, limit int) (int, error) {
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	var calls int
	err := s.DB.QueryRow(ctx, `SELECT COALESCE((SELECT calls FROM daily_usage WHERE day=$1),0)`, day).Scan(&calls)
	return max(0, limit-calls), err
}
