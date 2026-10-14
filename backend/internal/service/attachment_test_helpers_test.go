package service

import (
	"io"
	"os"
	"testing"
)

// openReader wraps a file at localPath as an io.Reader for the upload
// pipeline. Kept in a separate _test.go file so the production package
// doesn't pull in `os` just for this helper.
func openReader(t *testing.T, localPath string) io.Reader {
	t.Helper()
	f, err := os.Open(localPath)
	if err != nil {
		t.Fatalf("open %s: %v", localPath, err)
	}
	return f
}
