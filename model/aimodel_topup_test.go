package model

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestGrantAIModelTopup(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&AIModelTopupGrant{}))
	t.Cleanup(func() { DB.Exec("DELETE FROM ai_model_topup_grants") })
	user := User{Username: "aimodel-topup-test", Status: common.UserStatusEnabled, Quota: 200}
	require.NoError(t, DB.Create(&user).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&user) })
	token := Token{UserId: user.Id, Key: "aimodel-topup-test-token", Status: common.TokenStatusExhausted, ExpiredTime: -1, RemainQuota: -100}
	require.NoError(t, DB.Create(&token).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&token) })
	other := Token{UserId: user.Id + 1, Key: "aimodel-topup-other-token", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, DB.Create(&other).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&other) })

	result, err := GrantAIModelTopup("stripe:cs_test_001", user.Id, token.Id, 50)
	require.NoError(t, err)
	require.False(t, result.Duplicate)
	require.Equal(t, -100, result.BeforeQuota)
	require.Equal(t, -50, result.AfterQuota)
	require.NoError(t, DB.First(&token, token.Id).Error)
	require.Equal(t, -50, token.RemainQuota)
	require.Equal(t, common.TokenStatusExhausted, token.Status)
	require.NoError(t, DB.First(&user, user.Id).Error)
	require.Equal(t, 250, user.Quota)

	result, err = GrantAIModelTopup("stripe:cs_test_001", user.Id, token.Id, 50)
	require.NoError(t, err)
	require.True(t, result.Duplicate)
	require.Equal(t, -100, result.BeforeQuota)
	require.Equal(t, -50, result.AfterQuota)
	_, err = GrantAIModelTopup("stripe:cs_test_001", user.Id, token.Id, 51)
	require.ErrorIs(t, err, ErrAIModelTopupConflict)
	_, err = GrantAIModelTopup("stripe:cs_test_002", user.Id, other.Id, 50)
	require.ErrorIs(t, err, ErrAIModelTopupTarget)
	result, err = GrantAIModelTopup("stripe:cs_test_002", user.Id, token.Id, 100)
	require.NoError(t, err) // Failed ownership check rolled back its operation ID.
	require.Equal(t, -50, result.BeforeQuota)
	require.Equal(t, 50, result.AfterQuota)
	require.NoError(t, DB.First(&token, token.Id).Error)
	require.Equal(t, 50, token.RemainQuota)
	require.Equal(t, common.TokenStatusEnabled, token.Status)
	require.NoError(t, DB.First(&user, user.Id).Error)
	require.Equal(t, 350, user.Quota)

	_, err = GrantAIModelTopup("bad", user.Id, token.Id, 10)
	require.ErrorIs(t, err, ErrAIModelTopupInvalid)
	_, err = GrantAIModelTopup("stripe:cs_test_003", user.Id, token.Id, common.MaxWalletQuota)
	require.True(t, errors.Is(err, ErrAIModelTopupTarget)) // Atomic overflow guard.
	require.NoError(t, DB.First(&user, user.Id).Error)
	require.Equal(t, 350, user.Quota)
	var count int64
	require.NoError(t, DB.Model(&AIModelTopupGrant{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
}

func TestGrantAIModelTopupRetryRepairsCache(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&AIModelTopupGrant{}))
	user := User{Username: "aimodel-cache-repair", Status: common.UserStatusEnabled, AuthVersion: 1}
	require.NoError(t, DB.Create(&user).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&user) })
	token := Token{UserId: user.Id, Key: "aimodel-cache-repair-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5}
	require.NoError(t, DB.Create(&token).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&token) })
	t.Cleanup(func() { DB.Where("operation_id = ?", "stripe:cs_cache_repair").Delete(&AIModelTopupGrant{}) })
	_, err := GrantAIModelTopup("stripe:cs_cache_repair", user.Id, token.Id, 20)
	require.NoError(t, err)

	useUserCacheMiniRedis(t)
	user.Quota = 0 // A stale Redis snapshot from before the durable grant.
	require.NoError(t, populateUserCache(user))
	healthyClient := common.RDB
	brokenClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond, MaxRetries: 0})
	common.RDB = brokenClient
	_, err = GrantAIModelTopup("stripe:cs_cache_repair", user.Id, token.Id, 20)
	require.ErrorIs(t, err, ErrAIModelTopupCache)
	common.RDB = healthyClient
	require.NoError(t, brokenClient.Close())
	result, err := GrantAIModelTopup("stripe:cs_cache_repair", user.Id, token.Id, 20)
	require.NoError(t, err)
	assert.True(t, result.Duplicate)
	assert.Equal(t, 5, result.BeforeQuota)
	assert.Equal(t, 25, result.AfterQuota)
	quota, err := GetUserQuota(user.Id, false)
	require.NoError(t, err)
	assert.Equal(t, 20, quota)
	require.NoError(t, DB.First(&token, token.Id).Error)
	assert.Equal(t, 25, token.RemainQuota)
}

// Run this test against disposable real databases via TEST_MYSQL_DSN and
// TEST_POSTGRES_DSN. It exercises the unique operation key, row locks, and
// migration repeatability on their actual dialects.
func TestGrantAIModelTopupDialects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dsn    string
		open   func(string) gorm.Dialector
		dbType common.DatabaseType
	}{
		{"sqlite", "local", func(dsn string) gorm.Dialector { return sqlite.Open(dsn) }, common.DatabaseTypeSQLite},
		{"mysql", os.Getenv("TEST_MYSQL_DSN"), func(dsn string) gorm.Dialector { return mysql.Open(dsn) }, common.DatabaseTypeMySQL},
		{"postgres", os.Getenv("TEST_POSTGRES_DSN"), func(dsn string) gorm.Dialector { return postgres.Open(dsn) }, common.DatabaseTypePostgreSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.TrimSpace(tc.dsn) == "" {
				t.Skip("test database DSN is not configured")
			}
			dsn := tc.dsn
			if tc.dbType == common.DatabaseTypeSQLite {
				dsn = filepath.Join(t.TempDir(), "topup.db") + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"
			}
			db, err := gorm.Open(tc.open(dsn), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			if tc.dbType == common.DatabaseTypeSQLite {
				sqlDB.SetMaxOpenConns(1)
			}
			t.Cleanup(func() { assert.NoError(t, sqlDB.Close()) })
			previousDB := DB
			DB = db
			common.SetDatabaseTypes(tc.dbType, tc.dbType)
			initCol()
			t.Cleanup(func() {
				DB = previousDB
				common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
				initCol()
			})
			// Simulate the prior release's schema, then upgrade and restart.
			require.NoError(t, DB.AutoMigrate(&User{}, &Token{}))
			user := User{Username: "aimodel-topup-matrix", Status: common.UserStatusEnabled, Quota: 0}
			require.NoError(t, DB.Create(&user).Error)
			t.Cleanup(func() { DB.Unscoped().Delete(&user) })
			token := Token{UserId: user.Id, Key: "aimodel-topup-matrix-token", Status: common.TokenStatusExhausted, ExpiredTime: -1, RemainQuota: -10}
			require.NoError(t, DB.Create(&token).Error)
			t.Cleanup(func() { DB.Unscoped().Delete(&token) })
			for range 2 {
				require.NoError(t, migrateDB())
			}
			require.NoError(t, DB.First(&user, user.Id).Error)
			require.NoError(t, DB.First(&token, token.Id).Error)
			assert.Equal(t, -10, token.RemainQuota)
			t.Cleanup(func() {
				DB.Where("operation_id IN ?", []string{"stripe:cs_matrix_001", "stripe:cs_matrix_002"}).Delete(&AIModelTopupGrant{})
			})
			result, err := GrantAIModelTopup("stripe:cs_matrix_001", user.Id, token.Id, 20)
			require.NoError(t, err)
			assert.Equal(t, -10, result.BeforeQuota)
			assert.Equal(t, 10, result.AfterQuota)
			result, err = GrantAIModelTopup("stripe:cs_matrix_001", user.Id, token.Id, 20)
			require.NoError(t, err)
			assert.True(t, result.Duplicate)
			assert.Equal(t, -10, result.BeforeQuota)
			assert.Equal(t, 10, result.AfterQuota)
			var starts sync.WaitGroup
			starts.Add(1)
			results := make(chan AIModelTopupResult, 2)
			errors := make(chan error, 2)
			for range 2 {
				go func() {
					starts.Wait()
					r, err := GrantAIModelTopup("stripe:cs_matrix_002", user.Id, token.Id, 5)
					results <- r
					errors <- err
				}()
			}
			starts.Done()
			first, second := <-results, <-results
			require.NoError(t, <-errors)
			require.NoError(t, <-errors)
			assert.NotEqual(t, first.Duplicate, second.Duplicate)
			assert.Equal(t, 10, first.BeforeQuota)
			assert.Equal(t, 15, first.AfterQuota)
			assert.Equal(t, 10, second.BeforeQuota)
			assert.Equal(t, 15, second.AfterQuota)
			require.NoError(t, DB.First(&token, token.Id).Error)
			assert.Equal(t, 15, token.RemainQuota)
			require.NoError(t, DB.First(&user, user.Id).Error)
			assert.Equal(t, 25, user.Quota)
		})
	}
}
