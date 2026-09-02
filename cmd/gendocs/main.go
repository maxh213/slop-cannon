// Command gendocs writes the sc command reference (docs/cli.md) from the
// cobra command tree. `make docs-cli` runs it; a test in internal/cli fails
// when the committed file is stale.
//
// Usage:
//
//	go run ./cmd/gendocs [-o docs/cli.md]
//
// -o - writes to stdout.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"slop-cannon/internal/cli"
)

func main() {
	out := flag.String("o", cli.DocsPath, "output `FILE` (- for stdout)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "gendocs: no positional arguments expected")
		os.Exit(2)
	}
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

// run renders the reference and writes it to path (or stdout for "-").
func run(path string) error {
	var b bytes.Buffer
	if err := cli.WriteDocs(&b, cli.NewRootCommand(io.Discard)); err != nil {
		return err
	}
	if path == "-" {
		_, err := os.Stdout.Write(b.Bytes())
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}
