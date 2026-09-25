package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type aiModelTopupRequest struct {
	OperationID string `json:"operation_id" binding:"required"`
	TokenID     int    `json:"token_id" binding:"required"`
	Quota       int    `json:"quota" binding:"required"`
}

// AIModelTopup is a server-to-server endpoint for the configured shared user.
// No browser session, NewAPI admin token, or caller-supplied user ID is accepted.
func AIModelTopup(c *gin.Context) {
	secret := os.Getenv("AIMODEL_GATEWAY_TOPUP_SECRET")
	userID, userIDErr := strconv.Atoi(os.Getenv("AIMODEL_GATEWAY_USER_ID"))
	if len(secret) < 32 || userIDErr != nil || userID <= 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "AI Model top-up is not configured"})
		return
	}
	timestamp := c.GetHeader("X-AIModel-Timestamp")
	seconds, timestampErr := strconv.ParseInt(timestamp, 10, 64)
	now := time.Now().Unix()
	nonce := c.GetHeader("X-AIModel-Nonce")
	nonceBytes, nonceErr := hex.DecodeString(nonce)
	signatureBytes, signatureErr := hex.DecodeString(c.GetHeader("X-AIModel-Signature"))
	if timestampErr != nil || strconv.FormatInt(seconds, 10) != timestamp || seconds < now-300 || seconds > now+300 ||
		nonceErr != nil || len(nonce) != 32 || len(nonceBytes) != 16 || len(signatureBytes) != sha256.Size || signatureErr != nil {
		logger.LogWarn(c.Request.Context(), "AI Model top-up authentication rejected: reason=invalid_headers client_ip=%s", c.ClientIP())
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "unauthorized"})
		return
	}
	const maxTopupBodyBytes = 4096
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxTopupBodyBytes+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid request"})
		return
	}
	if len(body) > maxTopupBodyBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"success": false, "message": "request too large"})
		return
	}
	bodyHash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + nonce + "\n" + hex.EncodeToString(bodyHash[:])))
	if subtle.ConstantTimeCompare(signatureBytes, mac.Sum(nil)) != 1 {
		logger.LogWarn(c.Request.Context(), "AI Model top-up authentication rejected: reason=invalid_signature client_ip=%s", c.ClientIP())
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "unauthorized"})
		return
	}
	var req aiModelTopupRequest
	if err := common.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid request"})
		return
	}
	result, err := model.GrantAIModelTopup(req.OperationID, userID, req.TokenID, req.Quota)
	switch {
	case errors.Is(err, model.ErrAIModelTopupInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
	case errors.Is(err, model.ErrAIModelTopupConflict):
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error()})
	case errors.Is(err, model.ErrAIModelTopupTarget):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"success": false, "message": err.Error()})
	case errors.Is(err, model.ErrAIModelTopupCache):
		logger.LogWarn(c.Request.Context(), "AI Model top-up cache sync pending: operation_id=%s token_id=%d user_id=%d error=%v", req.OperationID, req.TokenID, userID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "retryable": true, "operation_id": req.OperationID, "message": "top-up cache synchronization pending"})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "top-up failed"})
	default:
		c.JSON(http.StatusOK, gin.H{
			"success": true, "duplicate": result.Duplicate, "operation_id": req.OperationID,
			"token_id": req.TokenID, "user_id": userID, "quota": req.Quota,
			"before_quota": result.BeforeQuota, "after_quota": result.AfterQuota,
		})
	}
}
