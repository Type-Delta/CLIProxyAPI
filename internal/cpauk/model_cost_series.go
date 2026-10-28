package cpauk

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (r *readerProxy) ModelCostSeries(ctx context.Context, query model.Query) (result model.ModelCostSeries, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	reader, ok := backend.(interface {
		ModelCostSeries(context.Context, model.Query) (model.ModelCostSeries, error)
	})
	if !ok {
		return result, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = reader.ModelCostSeries(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}
