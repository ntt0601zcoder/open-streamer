package ingest

import "path/filepath"

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", name)
}
