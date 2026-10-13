package orchestration

import (
	"context"
	"errors"

	"github.com/ycychome/ai-workflow/internal/config"
)

// PersistRetiredRepositoryEpoch changes only the already-retired repository's
// configuration fence. It does not invalidate any other repository or workflow.
func (e *Engine) PersistRetiredRepositoryEpoch(ctx context.Context, repositoryID int64, oldEpoch, newEpoch string, persist func(config.Config) error) error {
	state, err := e.Store.RepositoryMaintenanceStatus(ctx, repositoryID)
	if err != nil {
		return err
	}
	if !state.Retired || !state.IntakeStopped || state.Epoch != newEpoch || state.OldEpoch != oldEpoch || state.NewEpoch != newEpoch {
		return errors.New("仓库尚未按相同身份完成退休，不能更新配置代次")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cfg
	c.Repositories = append([]config.Repository(nil), c.Repositories...)
	found := false
	for i := range c.Repositories {
		if c.Repositories[i].ID == repositoryID {
			if c.Repositories[i].EffectiveEpoch() != oldEpoch && c.Repositories[i].EffectiveEpoch() != newEpoch {
				return errors.New("仓库配置代次已被其他操作改变")
			}
			c.Repositories[i].Epoch = newEpoch
			found = true
		}
	}
	if !found || persist == nil {
		return errors.New("缺少仓库或配置持久化函数")
	}
	if err = persist(c); err != nil {
		return err
	}
	e.cfg = c
	return nil
}
