package fprovider

import (
	"context"

	"github.com/terranodo/tegola"
)

type Feature struct {
	ID       uint64
	Geometry tegola.Geometry
	SRID     int
	Tags     map[string]interface{}
}

type FeatureProvider interface {
	TileFeatures(ctx context.Context, layerName string, tile tegola.TegolaTile) ([]Feature, error)
}
