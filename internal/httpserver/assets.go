package httpserver

import (
	"embed"
	"io/fs"
	"log"
)

//go:embed web
var embeddedAssets embed.FS

func mustAssetFS(logger *log.Logger) fs.FS {
	assetFS, err := fs.Sub(embeddedAssets, "web")
	if err != nil {
		logger.Fatalf("load embedded assets: %v", err)
	}
	return assetFS
}
