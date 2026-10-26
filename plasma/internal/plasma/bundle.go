package plasma

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxBundleBytes caps what one bundle may unpack to. Uploads are capped too,
// but a small gzip can expand without limit, and this runs as root on the
// engine's disk.
const maxBundleBytes = 8 << 30

// isBundleName reports whether a payload's name marks it as a bundle.
func isBundleName(name string) bool {
	for _, ext := range []string{".tar", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// extractBundle unpacks a tar or gzipped tar into dir, returning the bytes
// written. The archive comes from a remote client and plasmad is root, so
// only regular files, directories and symlinks are accepted, nothing may land
// outside dir, and nothing is ever written through a symlink: links are
// created last, after every file, and a path whose parent is a link is
// refused. A link's target is not resolved here — it is only ever followed
// inside the guest.
func extractBundle(src, dir string) (written int64, files int, err error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return 0, 0, fmt.Errorf("bundle: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	type link struct{ name, target string }
	var links []link
	var total int64
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return total, files, fmt.Errorf("bundle: %w", err)
		}
		name, err := bundleEntryName(h.Name)
		if err != nil {
			return total, files, err
		}
		if name == "" {
			continue // the archive's own "./"
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		files++
		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdirNoFollow(dir, name); err != nil {
				return total, files, err
			}
		case tar.TypeReg:
			if err := mkdirNoFollow(dir, path.Dir(name)); err != nil {
				return total, files, err
			}
			if total+h.Size > maxBundleBytes {
				return total, files, fmt.Errorf("bundle unpacks to more than %d bytes", int64(maxBundleBytes))
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			// O_EXCL: a duplicate entry must not overwrite, and a dst that is
			// already something else (a link from a hostile archive) fails.
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return total, files, fmt.Errorf("bundle: %s: %w", name, err)
			}
			n, err := io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			total += n
			if err != nil {
				return total, files, fmt.Errorf("bundle: %s: %w", name, err)
			}
		case tar.TypeSymlink:
			links = append(links, link{name, h.Linkname})
		default:
			return total, files, fmt.Errorf("bundle: %s: only files, directories and symlinks are allowed", name)
		}
	}
	for _, l := range links {
		if err := mkdirNoFollow(dir, path.Dir(l.name)); err != nil {
			return total, files, err
		}
		if err := os.Symlink(l.target, filepath.Join(dir, filepath.FromSlash(l.name))); err != nil {
			return total, files, fmt.Errorf("bundle: %s: %w", l.name, err)
		}
	}
	return total, files, nil
}

// bundleEntryName cleans an archive path and refuses one that would escape.
func bundleEntryName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("bundle: absolute path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("bundle: path %q leaves the bundle", name)
	}
	return clean, nil
}

// mkdirNoFollow creates rel under root one component at a time, refusing any
// component that already exists as something other than a real directory.
func mkdirNoFollow(root, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		switch {
		case err == nil && st.IsDir():
			continue
		case err == nil:
			return fmt.Errorf("bundle: %s is not a directory", strings.TrimPrefix(cur, root+"/"))
		case !os.IsNotExist(err):
			return err
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return err
		}
	}
	return nil
}
