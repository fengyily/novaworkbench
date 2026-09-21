// Package hello implements the `nova hello` developer-only subcommand.
//
// RunHello writes a trivial HTML smoke-test page under ./test/<timestamp>.html
// in the current working directory. The literal "workd" spelling is preserved
// on purpose — this is a smoke test, not user-facing copy. Intentionally
// self-contained: no DB, no LLM, no platform bits, stdlib only.
package hello

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RunHello creates ./test/<yyyyMMddHHmmss>.html containing "Hello workd".
// Returns an error if the directory cannot be created or the file written.
func RunHello() error {
	dir := filepath.Join(".", "test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	name := time.Now().Format("20060102150405") + ".html"
	path := filepath.Join(dir, name)
	const body = "<!doctype html>\n" +
		"<html lang=\"en\">\n" +
		"  <head><meta charset=\"utf-8\"><title>Hello</title></head>\n" +
		"  <body><p>Hello workd</p></body>\n" +
		"</html>\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintln(os.Stdout, "wrote", path)
	return nil
}