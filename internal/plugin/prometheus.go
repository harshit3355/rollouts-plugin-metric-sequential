package plugin

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type promQuerier struct{ api v1.API }

// NewPrometheusQuerier returns a Querier for a Prometheus-compatible HTTP API.
func NewPrometheusQuerier(address string) (Querier, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("address %q is not a URL", address)
	}
	c, err := api.NewClient(api.Config{Address: address})
	if err != nil {
		return nil, err
	}
	return promQuerier{v1.NewAPI(c)}, nil
}

// Query requires a scalar or at most one series. An empty result means no
// events in the window and reads as 0.
func (q promQuerier) Query(ctx context.Context, query string, at time.Time) (float64, error) {
	res, _, err := q.api.Query(ctx, query, at)
	if err != nil {
		return 0, err
	}
	var v float64
	switch r := res.(type) {
	case *model.Scalar:
		v = float64(r.Value)
	case model.Vector:
		switch len(r) {
		case 0:
			return 0, nil
		case 1:
			v = float64(r[0].Value)
		default:
			return 0, fmt.Errorf("query returned %d series; aggregate it, for example with sum()", len(r))
		}
	default:
		return 0, fmt.Errorf("unsupported result type %s", res.Type())
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("query returned %v", v)
	}
	return v, nil
}
