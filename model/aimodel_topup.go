package model

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrAIModelTopupInvalid  = errors.New("invalid AI Model top-up request")
	ErrAIModelTopupConflict = errors.New("AI Model top-up operation conflicts with an existing grant")
	ErrAIModelTopupTarget   = errors.New("AI Model top-up token or user is unavailable")
	ErrAIModelTopupCache    = errors.New("AI Model top-up cache synchronization failed")
	operationIDPattern      = regexp.MustCompile(`^[A-Za-z0-9:_-]{6,128}$`)
)

// AIModelTopupGrant is the durable idempotency record for a Gateway payment.
// Its operation ID is globally unique; this is not another spendable balance.
type AIModelTopupGrant struct {
	ID          uint   `gorm:"primaryKey"`
	OperationID string `gorm:"type:varchar(128);uniqueIndex;not null"`
	AttemptID   string `gorm:"type:char(36);not null"`
	TokenID     int    `gorm:"not null"`
	UserID      int    `gorm:"not null"`
	Quota       int    `gorm:"not null"`
	BeforeQuota int    `gorm:"not null"`
	AfterQuota  int    `gorm:"not null"`
	CreatedAt   time.Time
}

type AIModelTopupResult struct {
	Duplicate   bool
	BeforeQuota int
	AfterQuota  int
}

// GrantAIModelTopup credits both existing balances in a single transaction.
// A repeated operation succeeds only when its original target and amount match.
func GrantAIModelTopup(operationID string, userID, tokenID, quota int) (result AIModelTopupResult, err error) {
	if !operationIDPattern.MatchString(operationID) || userID <= 0 || tokenID <= 0 || quota <= 0 || quota > common.MaxWalletQuota {
		return result, ErrAIModelTopupInvalid
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		grant := AIModelTopupGrant{OperationID: operationID, AttemptID: uuid.NewString(), UserID: userID, TokenID: tokenID, Quota: quota}
		insert := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "operation_id"}}, DoNothing: true}).Create(&grant)
		if insert.Error != nil {
			return insert.Error
		}
		var persisted AIModelTopupGrant
		if err := lockForUpdate(tx).Where("operation_id = ?", operationID).First(&persisted).Error; err != nil {
			return err
		}
		if persisted.AttemptID != grant.AttemptID {
			if persisted.UserID != userID || persisted.TokenID != tokenID || persisted.Quota != quota {
				return ErrAIModelTopupConflict
			}
			result = AIModelTopupResult{Duplicate: true, BeforeQuota: persisted.BeforeQuota, AfterQuota: persisted.AfterQuota}
			return nil
		}

		var user User
		if err := tx.Select("id", "status").Where("id = ?", userID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAIModelTopupTarget
			}
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrAIModelTopupTarget
		}
		var token Token
		if err := lockForUpdate(tx).Select("id", "user_id", "key", "unlimited_quota", "remain_quota").Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAIModelTopupTarget
			}
			return err
		}
		if token.UnlimitedQuota {
			return ErrAIModelTopupTarget
		}
		if token.RemainQuota > common.MaxWalletQuota-quota {
			return ErrAIModelTopupTarget
		}
		if err := invalidateTokenCacheForMutation(token.Key); err != nil {
			return fmt.Errorf("%w: invalidate token before grant: %v", ErrAIModelTopupCache, err)
		}
		result.BeforeQuota = token.RemainQuota
		result.AfterQuota = token.RemainQuota + quota

		// Guard NewAPI's wallet ceiling while preserving negative token debt
		// and concurrent consumption. The quota columns are BIGINT on MySQL.
		tokenUpdate := tx.Model(&Token{}).Where("id = ? AND user_id = ? AND remain_quota <= ?", tokenID, userID, common.MaxWalletQuota-quota).
			Update("remain_quota", gorm.Expr("remain_quota + ?", quota))
		if tokenUpdate.Error != nil {
			return tokenUpdate.Error
		}
		if tokenUpdate.RowsAffected != 1 {
			return ErrAIModelTopupTarget
		}
		userUpdate := tx.Model(&User{}).Where("id = ? AND quota <= ?", userID, common.MaxWalletQuota-quota).
			Update("quota", gorm.Expr("quota + ?", quota))
		if userUpdate.Error != nil {
			return userUpdate.Error
		}
		if userUpdate.RowsAffected != 1 {
			return ErrAIModelTopupTarget
		}
		if err := tx.Model(&AIModelTopupGrant{}).Where("id = ?", persisted.ID).Updates(map[string]interface{}{
			"before_quota": result.BeforeQuota,
			"after_quota":  result.AfterQuota,
		}).Error; err != nil {
			return err
		}
		// Exhausted status is sticky in NewAPI. Re-enable only after debt is
		// fully repaid; leave disabled and expired tokens untouched.
		if err := tx.Model(&Token{}).Where("id = ? AND status = ? AND remain_quota > 0 AND (expired_time = -1 OR expired_time > ?)", tokenID, common.TokenStatusExhausted, common.GetTimestamp()).
			Update("status", common.TokenStatusEnabled).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	// On a retry the grant is already committed. Never increment cache again:
	// invalidate snapshots so the next read hydrates from the DB. This also
	// repairs an ambiguous cache write from the first attempt.
	if common.RedisEnabled {
		if result.Duplicate {
			if err := invalidateUserCache(userID); err != nil {
				return result, fmt.Errorf("%w: invalidate user: %v", ErrAIModelTopupCache, err)
			}
		} else if err := cacheIncrUserQuota(userID, int64(quota)); err != nil {
			// A failed cache write can be ambiguous. Deleting the hash is safe
			// whether the increment did or did not reach Redis.
			if clearErr := invalidateUserCache(userID); clearErr != nil {
				return result, fmt.Errorf("%w: user delta: %v; invalidate: %v", ErrAIModelTopupCache, err, clearErr)
			}
		}
		var token Token
		if err := DB.Unscoped().Select("key").Where("id = ?", tokenID).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) && result.Duplicate {
				return result, nil
			}
			return result, fmt.Errorf("%w: load token cache key: %v", ErrAIModelTopupCache, err)
		}
		if err := invalidateTokenCacheForMutation(token.Key); err != nil {
			return result, fmt.Errorf("%w: invalidate token after grant: %v", ErrAIModelTopupCache, err)
		}
	}
	return result, nil
}
