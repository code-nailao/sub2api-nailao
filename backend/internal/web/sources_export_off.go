//go:build !embed

package web

import "errors"

// ExportAssets is unavailable without the embedded frontend build.
func ExportAssets(string) (int, error) {
	return 0, errors.New("this binary was built without the embedded frontend")
}
