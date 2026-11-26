package server

import (
	"context"
	"io"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/asset"
)

// The asset methods of api.Backend. These are stubs until Task 11 replaces
// them with the asset store: the server has no assets directory yet, so
// every asset route answers asset.ErrOff (409 assets_off).

// Assets returns asset.ErrOff.
func (s *Server) Assets() ([]api.Asset, error) { return nil, asset.ErrOff }

// Asset returns asset.ErrOff.
func (s *Server) Asset(string) (api.Asset, io.ReadSeekCloser, error) {
	return api.Asset{}, nil, asset.ErrOff
}

// PutAsset returns asset.ErrOff without reading the body.
func (s *Server) PutAsset(context.Context, string, io.Reader, string, bool) (api.AssetPut, bool, error) {
	return api.AssetPut{}, false, asset.ErrOff
}

// DeleteAsset returns asset.ErrOff.
func (s *Server) DeleteAsset(context.Context, string, string) error { return asset.ErrOff }
