package store

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var (
	// ErrSteamIDTaken 表示该 Steam 账号已被另一个本站账号绑定。
	ErrSteamIDTaken = errors.New("store: steam account already linked by another user")
	// ErrAlreadyLinked 表示该本站账号已绑定了别的 Steam 账号。
	ErrAlreadyLinked = errors.New("store: user already linked to a different steam account")
	// ErrNotLinked 表示该用户没有有效绑定。
	ErrNotLinked = errors.New("store: user has no active steam link")
)

type LinkRepo struct{ db *gorm.DB }

func NewLinkRepo(db *gorm.DB) *LinkRepo { return &LinkRepo{db: db} }

func (r *LinkRepo) Link(ctx context.Context, userID, steamID uint64) error {
	now := time.Now().UTC()

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing SteamLink
		err := tx.Where("user_id = ?", userID).Take(&existing).Error

		switch {
		case err == nil:
			// 已有记录：只允许重新绑定同一个 Steam 账号
			if existing.SteamID != steamID {
				return ErrAlreadyLinked
			}
			return tx.Model(&SteamLink{}).Where("user_id = ?", userID).
				Updates(map[string]any{
					"unlinked_at": nil,
					"linked_at":   now,
					"updated_at":  now,
				}).Error

		case errors.Is(err, gorm.ErrRecordNotFound):
			link := SteamLink{
				UserID:          userID,
				SteamID:         steamID,
				VisibilityState: VisibilityUnknown,
				LinkedAt:        now,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			// active_steam_id 是生成列，Omit 掉避免 GORM 尝试写入
			if err := tx.Omit("ActiveSteamID").Create(&link).Error; err != nil {
				if isDuplicateKey(err) {
					return ErrSteamIDTaken
				}
				return err
			}
			return nil

		default:
			return err
		}
	})
}

func (r *LinkRepo) Unlink(ctx context.Context, userID uint64) error {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("user_id = ? AND unlinked_at IS NULL", userID).
		Updates(map[string]any{"unlinked_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotLinked
	}
	return nil
}

func (r *LinkRepo) ByUserID(ctx context.Context, userID uint64) (SteamLink, error) {
	var l SteamLink
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND unlinked_at IS NULL", userID).Take(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SteamLink{}, ErrNotLinked
	}
	return l, err
}

func (r *LinkRepo) UpdateVisibility(ctx context.Context, steamID uint64, state int8) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
		Updates(map[string]any{
			"visibility_state": state,
			"last_verified_at": now,
			"updated_at":       now,
		}).Error
}

// BumpPrivateStrikes 累加连续私密探测次数并返回新值。
// 连续 3 次后调用方应降级该用户的采集频率（见设计文档 §8.3）。
func (r *LinkRepo) BumpPrivateStrikes(ctx context.Context, steamID uint64) (int8, error) {
	var n int8
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SteamLink{}).
			Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
			UpdateColumn("private_strikes", gorm.Expr("private_strikes + 1")).Error; err != nil {
			return err
		}
		return tx.Model(&SteamLink{}).
			Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
			Select("private_strikes").Take(&n).Error
	})
	return n, err
}

func (r *LinkRepo) ResetPrivateStrikes(ctx context.Context, steamID uint64) error {
	return r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("steam_id64 = ? AND unlinked_at IS NULL", steamID).
		UpdateColumn("private_strikes", 0).Error
}

func (r *LinkRepo) ActiveSteamIDs(ctx context.Context) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("unlinked_at IS NULL").
		Order("steam_id64").
		Pluck("steam_id64", &ids).Error
	return ids, err
}

// StaleSteamIDs 返回距上次成功校准已超过阈值的活跃用户。
//
// 每日校准据此过滤，避免 worker 每次重启都把全量用户重新排进队列。
// last_verified_at 为 NULL 表示从未校准过（刚绑定），一律纳入。
func (r *LinkRepo) StaleSteamIDs(ctx context.Context, before time.Time) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&SteamLink{}).
		Where("unlinked_at IS NULL").
		Where("last_verified_at IS NULL OR last_verified_at < ?", before).
		Order("steam_id64").
		Pluck("steam_id64", &ids).Error
	return ids, err
}

// isDuplicateKey 识别 MySQL 的 1062 错误码。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
