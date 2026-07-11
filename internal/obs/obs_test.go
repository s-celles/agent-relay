package obs

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestMetricsRecordUsage(t *testing.T) {
	m := NewMetrics()
	m.RecordUsage(10, 20, 0.02)
	m.RecordUsage(5, 5, 0.01)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))

	var got struct {
		InputTokens  int64   `json:"input_tokens_total"`
		OutputTokens int64   `json:"output_tokens_total"`
		CostUSD      float64 `json:"cost_usd_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if got.InputTokens != 15 || got.OutputTokens != 25 {
		t.Errorf("tokens = %+v, want 15/25", got)
	}
	if got.CostUSD < 0.0299 || got.CostUSD > 0.0301 {
		t.Errorf("cost_usd_total = %v, want ~0.03", got.CostUSD)
	}
}

func TestMetricsCostAbsentWhenUnreported(t *testing.T) {
	m := NewMetrics()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["cost_usd_total"] != float64(0) {
		t.Errorf("cost_usd_total = %v, want 0", got["cost_usd_total"])
	}
}
