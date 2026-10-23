package management

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	log "github.com/sirupsen/logrus"
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

func parseUsageTZ(c *gin.Context) (int, bool) {
	tz, _ := strconv.Atoi(strings.TrimSpace(c.Query("tz_offset")))
	if tz < -14*60 || tz > 14*60 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tz_offset must be within ±840 minutes"})
		return 0, false
	}
	return tz, true
}

// GetUsageStoreStats returns aggregated usage and estimated cost for a time range.
// Query: from, to (unix seconds or RFC3339; default last 24h), bucket (hour|day), tz_offset (minutes east of UTC),
// and optional model, provider, api_key_id, auth_index filters.
func (h *Handler) GetUsageStoreStats(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	from, to, ok := parseUsageRange(c)
	if !ok {
		return
	}
	tz, ok := parseUsageTZ(c)
	if !ok {
		return
	}
	filter := usagestore.StatsFilter{
		Model:     strings.TrimSpace(c.Query("model")),
		Provider:  strings.TrimSpace(c.Query("provider")),
		APIKeyID:  strings.TrimSpace(c.Query("api_key_id")),
		AuthIndex: strings.TrimSpace(c.Query("auth_index")),
	}
	stats, err := s.Stats(c.Request.Context(), from, to, c.Query("bucket"), tz, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// parseUsageEventFilter reads from, to, model, provider, api_key_id, auth_index, failed, before and limit.
func parseUsageEventFilter(c *gin.Context) (usagestore.EventFilter, bool) {
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
		return f, false
	}
	if v := strings.TrimSpace(c.Query("failed")); v != "" {
		failed, errBool := strconv.ParseBool(v)
		if errBool != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed must be true or false"})
			return f, false
		}
		f.Failed = &failed
	}
	if v := strings.TrimSpace(c.Query("substituted")); v != "" {
		substituted, errBool := strconv.ParseBool(v)
		if errBool != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "substituted must be true or false"})
			return f, false
		}
		f.Substituted = &substituted
	}
	f.BeforeID, _ = strconv.ParseInt(strings.TrimSpace(c.Query("before")), 10, 64)
	f.Limit, _ = strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	return f, true
}

// GetUsageStoreEvents returns stored requests newest first.
// Query: from, to, model, provider, api_key_id, auth_index, failed (true|false), before (event id), limit (<=500).
func (h *Handler) GetUsageStoreEvents(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	f, ok := parseUsageEventFilter(c)
	if !ok {
		return
	}
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

var usageExportColumns = []string{"id", "time", "request_id", "provider", "executor_type", "model", "alias", "endpoint",
	"auth_type", "auth_index", "source", "api_key", "access_provider", "reasoning_effort", "service_tier", "stream",
	"failed", "status_code", "error", "latency_ms", "ttft_ms", "input_uncached", "cache_read", "cache_write", "output",
	"reasoning", "unclassified", "total", "cost_usd", "priced", "client_ip", "user_agent", "response_model", "substituted"}

// sanitizeCSVCell prefixes a single quote to string cell values starting with formula triggers
// (=, +, -, @, tab, CR) per OWASP CSV injection guidelines.
func sanitizeCSVCell(v string) string {
	if len(v) > 0 {
		switch v[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + v
		}
	}
	return v
}

// ExportUsageStoreEvents downloads matching events (up to usagestore.MaxExportEvents) as CSV or JSON.
// Query: the events filters plus format (csv|json, default csv).
func (h *Handler) ExportUsageStoreEvents(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	f, ok := parseUsageEventFilter(c)
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "csv")))
	if format != "csv" && format != "json" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "format must be csv or json"})
		return
	}
	name := "usage-events-" + time.Now().Format("20060102-150405") + "." + format
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	ctx := c.Request.Context()
	if format == "json" {
		c.Header("Content-Type", "application/json")
		c.Status(http.StatusOK)
		enc := json.NewEncoder(c.Writer)
		first := true
		_, _ = c.Writer.WriteString("[")
		errExport := s.ExportEvents(ctx, f, func(e *usagestore.Event) error {
			if !first {
				if _, errWrite := c.Writer.WriteString(","); errWrite != nil {
					return errWrite
				}
			}
			first = false
			return enc.Encode(e)
		})
		_, _ = c.Writer.WriteString("]")
		if errExport != nil {
			log.Warnf("usage export interrupted: %v", errExport)
		}
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Status(http.StatusOK)
	// UTF-8 BOM so spreadsheet apps detect the encoding.
	_, _ = c.Writer.WriteString("\ufeff")
	w := csv.NewWriter(c.Writer)
	_ = w.Write(usageExportColumns)
	errExport := s.ExportEvents(ctx, f, func(e *usagestore.Event) error {
		t := e.Tokens
		return w.Write([]string{
			strconv.FormatInt(e.ID, 10), time.UnixMilli(e.Timestamp).UTC().Format(time.RFC3339Nano), sanitizeCSVCell(e.RequestID),
			sanitizeCSVCell(e.Provider), sanitizeCSVCell(e.ExecutorType), sanitizeCSVCell(e.Model), sanitizeCSVCell(e.Alias), sanitizeCSVCell(e.Endpoint),
			sanitizeCSVCell(e.AuthType), sanitizeCSVCell(e.AuthIndex), sanitizeCSVCell(e.Source), sanitizeCSVCell(e.APIKey),
			sanitizeCSVCell(e.AccessProvider), sanitizeCSVCell(e.ReasoningEffort), sanitizeCSVCell(e.ServiceTier), strconv.FormatBool(e.Stream),
			strconv.FormatBool(e.Failed), strconv.Itoa(e.StatusCode), sanitizeCSVCell(e.Error), strconv.FormatInt(e.LatencyMs, 10),
			strconv.FormatInt(e.TTFTMs, 10), strconv.FormatInt(t.InputUncached, 10), strconv.FormatInt(t.CacheRead, 10),
			strconv.FormatInt(t.CacheWrite, 10), strconv.FormatInt(t.Output, 10), strconv.FormatInt(t.Reasoning, 10),
			strconv.FormatInt(t.Unclassified, 10), strconv.FormatInt(t.Total, 10),
			strconv.FormatFloat(e.Cost, 'f', -1, 64), strconv.FormatBool(e.Priced), sanitizeCSVCell(e.ClientIP), sanitizeCSVCell(e.UserAgent),
			sanitizeCSVCell(e.ResponseModel), strconv.FormatBool(e.Substituted),
		})
	})
	w.Flush()
	if errExport == nil {
		errExport = w.Error()
	}
	if errExport != nil {
		log.Warnf("usage export interrupted: %v", errExport)
	}
}

// GetUsageStoreAnalysis returns latency percentiles, a latency histogram, per-model latency and a
// weekday x hour heatmap. Query: from, to, tz_offset (as for stats).
func (h *Handler) GetUsageStoreAnalysis(c *gin.Context) {
	s := currentUsageStore(c)
	if s == nil {
		return
	}
	from, to, ok := parseUsageRange(c)
	if !ok {
		return
	}
	tz, ok := parseUsageTZ(c)
	if !ok {
		return
	}
	analysis, err := s.Analysis(c.Request.Context(), from, to, tz)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, analysis)
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
