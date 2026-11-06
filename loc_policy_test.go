package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This filesystem walk deliberately includes untracked code and testdata tools.
// Keep enforcement in tests so it adds no runtime code or dependencies.
func TestFileLengths(t *testing.T) {
	if err := checkFileLengths(".", func(message string) { t.Log(message) }); err != nil {
		t.Fatal(err)
	}
}

func checkFileLengths(root string, warn func(string)) error {
	var failures []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel != "." {
				switch entry.Name() {
				case ".git", "vendor":
					return filepath.SkipDir
				}
			}
			return nil
		}
		rel = filepath.ToSlash(rel)
		goFile := strings.HasSuffix(rel, ".go")
		workflow := strings.HasPrefix(rel, ".github/workflows/") &&
			(strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"))
		if !goFile && !workflow && rel != ".no-mistakes.yaml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if goFile {
			header, err := parser.ParseFile(token.NewFileSet(), path, data, parser.PackageClauseOnly|parser.ParseComments)
			if err != nil {
				return err
			}
			if ast.IsGenerated(header) {
				return nil
			}
		}
		lines := physicalLines(data)
		switch {
		case lines > 1000:
			failures = append(failures, fmt.Sprintf("%s: %d lines exceeds hard limit 1000 (target 500)", rel, lines))
		case lines > 800:
			warn(fmt.Sprintf("warning: %s: %d lines exceeds soft limit 800 (target 500)", rel, lines))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(failures) != 0 {
		return fmt.Errorf("file length policy:\n%s", strings.Join(failures, "\n"))
	}
	return nil
}

func physicalLines(data []byte) int {
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) != 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}
