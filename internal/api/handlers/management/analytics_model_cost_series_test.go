package management

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (r *analyticsV2ReaderFake) ModelCostSeries(_ context.Context, query model.Query) (model.ModelCostSeries, error) {
	r.query = query
	return model.ModelCostSeries{Model: "gpt-test", Buckets: []model.ModelCostSeriesBucket{}}, r.err
}

func TestPostAnalyticsModelCostSeriesV8(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	handler := v2HandlerWithReader(reader, nil)
	body := `{"schema_version":2,"operation":"model_cost_series","start":"2026-08-01T00:00:00Z","end":"2026-08-01T01:00:00Z","time_zone":"Asia/Bangkok","filters":{"model":["gpt-test"],"provider":["codex"]},"bucket_width":"1h"}`
	ctx, recorder := v2Request(http.MethodPost, "/v8/management/analytics/query", body)
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if reader.query.Operation != model.OperationModelCostSeries || reader.query.TimeZone != "Asia/Bangkok" {
		t.Fatalf("dispatched %+v", reader.query)
	}
}
