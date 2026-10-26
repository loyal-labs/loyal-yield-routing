package webserve

import (
	"errors"
	"io/fs"
	"strings"
)

// WithAssetFallback keeps old tabs' hashed assets available after a release.
// HTML and public files always belong to the current embedded build.
func WithAssetFallback(dist, assets fs.FS) fs.FS {
	return assetFallback{dist: dist, assets: assets}
}

type assetFallback struct {
	dist, assets fs.FS
}

func (f assetFallback) Open(name string) (fs.File, error) {
	file, err := f.dist.Open(name)
	if errors.Is(err, fs.ErrNotExist) && fs.ValidPath(name) && strings.HasPrefix(name, "assets/") {
		return f.assets.Open(strings.TrimPrefix(name, "assets/"))
	}
	return file, err
}
