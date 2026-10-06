package upgrade

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/tracepad/tracepad/internal/store"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dataDBName is the database's file in a data directory.
const dataDBName = "tracepad.db"

// survey walks a data directory before anything stops: its size, for the
// room check, and whether it holds only what an archive takes back whole —
// directories and regular files (Decision 7).
func survey(dataDir string) (bytes int64, err error) {
	err = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return nil
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
			return nil
		}
		return fmt.Errorf("%s is neither a file nor a directory, which the backup does not take", path)
	})
	return bytes, err
}

// Archived is what the archive's read-back must find.
type Archived struct {
	SHA256 string `json:"sha256"`
	// DBSize is tracepad.db's size when it was archived; -1 when the archive
	// was written by another program (a container's) and only its presence
	// is checked.
	DBSize int64 `json:"db_size"`
}

// writeArchive writes dataDir's contents to path as a gzipped tar, entries
// named `./…` as `tar -C dir .` names them, and hashes the bytes as they are
// written.
func writeArchive(dataDir, path string) (Archived, error) {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Archived{}, err
	}
	defer out.Close()
	hash := sha256.New()
	buf := bufio.NewWriterSize(io.MultiWriter(out, hash), 1<<20)
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	dbSize := int64(-1)
	err = filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s is neither a file nor a directory", p)
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = "./" + filepath.ToSlash(rel)
		if rel == "." {
			h.Name = "./"
		} else if info.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, f)
		f.Close() // ignored: a file read whole, or not, as io.Copy says
		if err != nil {
			return err
		}
		if n != info.Size() {
			return fmt.Errorf("%s changed while it was archived", p)
		}
		if rel == dataDBName {
			dbSize = n
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = buf.Flush()
	}
	if err == nil {
		err = syncFile(out)
	}
	if err != nil {
		return Archived{}, err
	}
	if dbSize < 0 {
		return Archived{}, fmt.Errorf("%s has no %s", dataDir, dataDBName)
	}
	return Archived{SHA256: hex.EncodeToString(hash.Sum(nil)), DBSize: dbSize}, nil
}

// verifyArchive reads an archive back whole: its bytes hash to what was
// written (when that is known), the gzip stream and the tar run to their
// ends, and tracepad.db is in it, at its size. A truncated archive fails here,
// before anything trusts it (Decision 8).
func verifyArchive(path string, want Archived) error {
	_, err := readBack(path, want)
	return err
}

// ReadBack is what an archive's read-back measured: the digest of its bytes,
// and the size its files take extracted.
type ReadBack struct {
	SHA256 string
	Bytes  int64
}

// readBack is verifyArchive, answering what it measured: the digest it
// computes on the way (so the bytes are read once) and the room a restore
// needs.
func readBack(path string, want Archived) (ReadBack, error) {
	f, err := os.Open(path)
	if err != nil {
		return ReadBack{}, err
	}
	defer f.Close()
	hash := sha256.New()
	gz, err := gzip.NewReader(bufio.NewReaderSize(io.TeeReader(f, hash), 1<<20))
	if err != nil {
		return ReadBack{}, fmt.Errorf("%s does not read as gzip: %w", path, err)
	}
	tr := tar.NewReader(gz)
	found := int64(-1)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ReadBack{}, fmt.Errorf("%s does not read back whole: %w", path, err)
		}
		n, err := io.Copy(io.Discard, tr)
		total += n
		if err != nil {
			return ReadBack{}, fmt.Errorf("%s does not read back whole: %w", path, err)
		}
		if cleanEntry(h.Name) == dataDBName && h.Typeflag == tar.TypeReg {
			found = n
		}
	}
	// The gzip trailer is checked at the end of the stream; what follows the
	// tar's end must be read for it.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return ReadBack{}, fmt.Errorf("%s does not read back whole: %w", path, err)
	}
	if _, err := io.Copy(hash, f); err != nil {
		return ReadBack{}, err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	switch {
	case found < 0:
		return ReadBack{}, fmt.Errorf("%s has no %s in it", path, dataDBName)
	case found == 0:
		return ReadBack{}, fmt.Errorf("the %s in %s is empty", dataDBName, path)
	case want.DBSize >= 0 && found != want.DBSize:
		return ReadBack{}, fmt.Errorf("the %s in %s is %d bytes, not the %d it was", dataDBName, path, found, want.DBSize)
	case want.SHA256 != "" && sum != want.SHA256:
		return ReadBack{}, fmt.Errorf("%s is not the archive that was written: its checksum changed", path)
	}
	return ReadBack{SHA256: sum, Bytes: total}, nil
}

// cleanEntry is a tar entry's name without its `./`.
func cleanEntry(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(name), "./"), "/")
}

// dirMode is a directory's mode, applied once its contents are written.
type dirMode struct {
	path string
	mode os.FileMode
}

// safeJoin is where an archive's entry goes under dest, or an error when it
// would go anywhere else: an absolute name, `..`, or a name that leaves dest
// once joined and cleaned.
func safeJoin(dest, name string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(cleanEntry(name)))
	if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("the archive names %q, outside its directory", name)
	}
	root := filepath.Clean(dest)
	target := filepath.Join(root, rel)
	if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("the archive names %q, outside its directory", name)
	}
	return target, nil
}

// extractArchive restores an archive into dest, which must not exist. Only
// directories and regular files are made, and no entry may leave dest.
func extractArchive(path, dest string, mode os.FileMode) error {
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	modes := []dirMode{{dest, mode}}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// The deepest first: a directory's own mode can take away the
			// write its children's need.
			for i := len(modes) - 1; i >= 0; i-- {
				if err := os.Chmod(modes[i].path, modes[i].mode); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := cleanEntry(h.Name)
		if name == "" || name == "." {
			continue
		}
		target, err := safeJoin(dest, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			// Its mode once everything in it is written: one archived
			// without its owner's write would refuse its own files (the
			// tenth review).
			modes = append(modes, dirMode{target, os.FileMode(h.Mode).Perm()})
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode).Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			_ = os.Chtimes(target, h.ModTime, h.ModTime) // ignored: a time of modification is not the data
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("the archive holds %q, a link to %q, which a backup never has", h.Name, h.Linkname)
		default:
			return fmt.Errorf("the archive holds %q, which is neither a file nor a directory", h.Name)
		}
	}
}

// quickCheck runs SQLite's quick_check over a restored database.
func quickCheck(ctx context.Context, dbPath string) error {
	db, err := sql.Open("sqlite", store.FileURI(dbPath))
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("%s does not open as a database: %w", dbPath, err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s fails its check: %s", dbPath, strings.Join(problems, "; "))
	}
	return nil
}
