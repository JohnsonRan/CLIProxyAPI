package management

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

const usageStoreDisabled = "usage_store_disabled"

func currentUsageStore(c *gin.Context) *usagestore.Store {
	s := usagestore.Current()
	if s == nil {
		c.JSON(http.StatusConflict, gin.H{"error": usageStoreDisabled, "message": "enable observability.usage.store to record usage"})
	}
	return s
}

// parseUsageTime accepts unix seconds or RFC3339; empty returns def.
func parseUsageTime(value string, def time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return def, nil
	}
	if sec, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(sec, 0), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q: use unix seconds or RFC3339", value)
	}
	return t, nil
}

func parseUsageRange(c *gin.Context) (time.Time, time.Time, bool) {
	to, errTo := parseUsageTime(c.Query("to"), time.Now())
	if errTo != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errTo.Error()})
		return time.Time{}, time.Time{}, false
	}
	from, errFrom := parseUsageTime(c.Query("from"), to.Add(-24*time.Hour))
	if errFrom != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errFrom.Error()})
		return time.Time{}, time.Time{}, false
	}
	if !from.Before(to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must be before to"})
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// GetUsageStoreStats returns aggregated usage and estimated cost for a time range.
// Query: from, to (unix seconds or RFC3339; default last 24h), bucket (hour|day), tz_offset (minutes east of UTC).
func (h *Handler) GetUsageStoreStats(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	from, to, ok := parseUsageRange(c)
	if !ok {
		return
	}
	tz, _ := strconv.Atoi(strings.TrimSpace(c.Query("tz_offset")))
	if tz < -14*60 || tz > 14*60 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tz_offset must be within ±840 minutes"})
		return
	}
	stats, err := s.Stats(c.Request.Context(), from, to, c.Query("bucket"), tz)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// GetUsageStoreEvents returns stored requests newest first.
// Query: from, to, model, provider, api_key_id, auth_index, failed (true|false), before (event id), limit (<=500).
func (h *Handler) GetUsageStoreEvents(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	f := usagestore.EventFilter{
		Model:     strings.TrimSpace(c.Query("model")),
		Provider:  strings.TrimSpace(c.Query("provider")),
		APIKeyID:  strings.TrimSpace(c.Query("api_key_id")),
		AuthIndex: strings.TrimSpace(c.Query("auth_index")),
	}
	var err error
	if f.From, err = parseUsageTime(c.Query("from"), time.Time{}); err == nil {
		f.To, err = parseUsageTime(c.Query("to"), time.Time{})
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if v := strings.TrimSpace(c.Query("failed")); v != "" {
		failed, errBool := strconv.ParseBool(v)
		if errBool != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed must be true or false"})
			return
		}
		f.Failed = &failed
	}
	f.BeforeID, _ = strconv.ParseInt(strings.TrimSpace(c.Query("before")), 10, 64)
	f.Limit, _ = strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	events, err := s.Events(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var next int64
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{"events": events, "next_before": next})
}

// GetUsageStorePricing lists the effective price of every stored model.
func (h *Handler) GetUsageStorePricing(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	writeUsagePricing(c, s)
}

// RefreshUsageStorePricing downloads models.dev prices now.
func (h *Handler) RefreshUsageStorePricing(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
	defer cancel()
	if err := s.RefreshPrices(ctx); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	writeUsagePricing(c, s)
}

func writeUsagePricing(c *gin.Context, s *usagestore.Store) {
	models, err := s.ModelPrices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	updatedAt, enabled, count, lastError := s.Pricer().Status()
	modelsDev := gin.H{"enabled": enabled, "count": count, "last_error": lastError, "updated_at": nil}
	if !updatedAt.IsZero() {
		modelsDev["updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, gin.H{"models_dev": modelsDev, "models": models})
}
