// Package templates is what agk brick init writes: the brick templates of agentiik/brick-sdk, one
// directory per language, copied here by copy.sh and embedded in the binary.
//
// They are Apache-2.0, as brick-sdk is, and a brick written from them is its author's under any
// licence: the files are a starting point handed over, not a part of agk that stays linked to it.
package templates

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed all:go all:python all:typescript
var files embed.FS

// Languages are the languages there is a template for, in the order agk lists them.
func Languages() []string { return []string{"go", "python", "typescript"} }

// name is a brick name as the manifest takes it, ^[a-z0-9]+(?:-[a-z0-9]+)*$: a person types it,
// the catalog files a page under it and the image is published under it.
var name = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ErrName is a name the manifest would refuse.
var ErrName = errors.New("a brick name is lowercase letters and digits, words joined by single hyphens, as metadata.name takes it")

// ErrExists is a directory that already holds something, which agk brick init never writes into.
var ErrExists = errors.New("the directory exists and is not empty")

// Write writes the template of lang into dir, every {{name}} replaced by brick, and answers the
// files it wrote, relative to dir. dir must not exist, or be empty: a scaffold written over a
// brick in progress would overwrite its manifest and its cases without a word.
func Write(lang, brick, dir string) ([]string, error) {
	if !name.MatchString(brick) || len(brick) > 63 {
		return nil, fmt.Errorf("%q: %w", brick, ErrName)
	}
	known := false
	for _, l := range Languages() {
		known = known || l == lang
	}
	if !known {
		return nil, fmt.Errorf("no template in %q: there is one in %s", lang, strings.Join(Languages(), ", "))
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s: %w", dir, ErrExists)
	}
	var written []string
	err := fs.WalkDir(files, lang, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, lang+"/"), ".tmpl")
		to := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(to, []byte(strings.ReplaceAll(string(b), "{{name}}", brick)), 0o644); err != nil {
			return err
		}
		written = append(written, path.Clean(rel))
		return nil
	})
	sort.Strings(written)
	return written, err
}
