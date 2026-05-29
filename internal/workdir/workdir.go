// Package workdir packs a client's working directory into a tar.gz and unpacks
// it on the provider. Code travels client→agent directly (P2P over the
// tailnet); the coordinator never carries the payload.
//
// v1 assumes a small CODE directory (the eng-review foot-gun note): there is a
// size cap and a skip list for the obvious heavy/irrelevant dirs so a stray
// dataset or checkpoint does not balloon the transfer.
package workdir

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxUncompressedBytes bounds both packing and unpacking. Past this, Tar/Untar
// error rather than ship/expand a giant dataset (foot-gun guard, M1).
const MaxUncompressedBytes = 256 << 20 // 256 MiB

// skipDirs are never packed: VCS metadata, virtualenvs, caches, and the usual
// heavy outputs. Keeps "code dir only" honest without a full .gpignore parser.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "__pycache__": true,
	".venv": true, "venv": true, "env": true,
	".mypy_cache": true, ".pytest_cache": true,
	"wandb": true, "checkpoints": true, "data": true, "datasets": true,
}

// Tar writes srcDir as a gzip-compressed tarball to w. Symlinks are stored as
// symlinks; skipDirs are pruned; the running total is capped.
func Tar(srcDir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var total int64
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && skipDirs[info.Name()] && path != srcDir {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		total += info.Size()
		if total > MaxUncompressedBytes {
			return fmt.Errorf("workdir exceeds %d bytes; v1 expects a code dir only (exclude datasets/checkpoints)", MaxUncompressedBytes)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Untar extracts a gzip tarball from r into destDir. It rejects entries that
// would escape destDir (zip-slip) and caps the total expanded size.
func Untar(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	cleanDest := filepath.Clean(destDir)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		target, err := safeJoin(cleanDest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links entirely. A symlink created earlier in the archive can
			// redirect a later regular-file write outside destDir (symlink
			// TOCTOU) even when each entry's lexical path passes safeJoin. Code
			// workdirs rarely need links, so dropping them closes the escape
			// class outright rather than trying to validate it.
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			total += hdr.Size
			if total > MaxUncompressedBytes {
				return fmt.Errorf("archive exceeds %d bytes", MaxUncompressedBytes)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

// safeJoin joins dest and a tar entry name, rejecting absolute paths and any
// result that escapes dest (the zip-slip defense).
func safeJoin(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute path in archive: %q", name)
	}
	target := filepath.Join(dest, name)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workdir: %q", name)
	}
	return target, nil
}
