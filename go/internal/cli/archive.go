package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Cloudflare's limit on a request body on most plans.
const uploadLimit = 100 << 20

// packDir writes a tar.gz of the files under dir, named relative to it, to w.
// files lists them (slash-separated, as git prints them); nil means all of
// dir. Directories in the list are packed whole. Returns the number of
// entries and their total size.
func packDir(w io.Writer, dir string, files []string) (int, int64, error) {
	// WalkDir doesn't descend into a root that is a symlink.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	n, size := 0, int64(0)
	add := func(path string, d fs.DirEntry) error {
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // deleted since git listed it
			}
			return err
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.Mode().IsRegular(), info.IsDir():
		default:
			return nil // sockets, devices and pipes stay behind
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname, h.Uid, h.Gid = "", "", 0, 0
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		n++
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			written, err := io.Copy(tw, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			size += written
		}
		return nil
	}
	walk := func(root string) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return add(path, d)
		})
	}
	var err error
	if files == nil {
		err = walk(dir)
	} else {
		seen := map[string]bool{}
		for _, f := range files {
			// Parent directories first, so they unpack with their own modes.
			parts := strings.Split(f, "/")
			for i := 1; i < len(parts) && err == nil; i++ {
				p := strings.Join(parts[:i], "/")
				if !seen[p] {
					seen[p] = true
					err = add(filepath.Join(dir, filepath.FromSlash(p)), nil)
				}
			}
			if err != nil {
				break
			}
			path := filepath.Join(dir, filepath.FromSlash(f))
			if info, e := os.Lstat(path); e == nil && info.IsDir() {
				err = walk(path) // a submodule
			} else {
				err = add(path, nil)
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return 0, 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, 0, err
	}
	return n, size, gz.Close()
}

// gitFiles lists the files git would commit under dir: tracked ones and new
// ones it doesn't ignore. ok is false outside a git work tree.
func gitFiles(dir string) (files []string, ok bool, err error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false, nil
	}
	check := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if out, err := check.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil, false, nil
	}
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, true, fmt.Errorf("listing the files in %s with git: %w", dir, err)
	}
	seen := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, true, nil
}

// unpack extracts a tar.gz into dir, which it makes. The archive comes from
// a sandbox, so nothing in it may reach outside dir: entries can't land
// outside it or be written through a symlink, symlinks may only point inside
// it, an existing directory is never replaced, and .git directories, whose
// hooks and config can run commands, are left out. Returns how many entries
// it unpacked and how many it left out.
func unpack(r io.Reader, dir string) (n, skipped int, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("reading the archive: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, skipped, nil
		}
		if err != nil {
			return n, skipped, fmt.Errorf("reading the archive: %w", err)
		}
		name := filepath.FromSlash(strings.TrimPrefix(h.Name, "./"))
		name = strings.TrimSuffix(name, string(filepath.Separator))
		if name == "" || name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return n, skipped, fmt.Errorf("the archive has an entry outside its directory: %s", h.Name)
		}
		if hasGitDir(name) {
			skipped++
			continue
		}
		target := filepath.Join(dir, name)
		if err := noLinksBetween(dir, filepath.Dir(target)); err != nil {
			return n, skipped, err
		}
		existing, lerr := os.Lstat(target)
		exists := lerr == nil
		isLink := exists && existing.Mode()&fs.ModeSymlink != 0
		isDir := exists && existing.IsDir()
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if isLink || exists && !isDir {
				if err := os.Remove(target); err != nil {
					return n, skipped, err
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, skipped, err
			}
			if info, err := os.Lstat(target); err == nil && info.IsDir() {
				_ = os.Chmod(target, mode|0o700)
			}
		case tar.TypeReg:
			if isDir {
				return n, skipped, fmt.Errorf("the archive has a file %s where there's a directory; not replacing it", target)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, skipped, err
			}
			if isLink || exists && !existing.Mode().IsRegular() {
				if err := os.Remove(target); err != nil {
					return n, skipped, err
				}
			}
			// O_NOFOLLOW: a symlink made since the check isn't followed.
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|oNoFollow, mode)
			if err != nil {
				return n, skipped, err
			}
			_, err = io.Copy(f, tr)
			if err == nil {
				err = f.Chmod(mode)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, skipped, err
			}
			_ = os.Chtimes(target, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			if !linkStaysIn(dir, target, h.Linkname) {
				skipped++
				continue
			}
			if isDir {
				return n, skipped, fmt.Errorf("the archive has a symlink %s where there's a directory; not replacing it", target)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, skipped, err
			}
			if exists {
				if err := os.Remove(target); err != nil {
					return n, skipped, err
				}
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return n, skipped, err
			}
		default:
			skipped++ // hard links, devices and the like stay behind
			continue
		}
		n++
	}
}

func hasGitDir(name string) bool {
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

// linkStaysIn says whether a symlink at target pointing at linkname resolves
// inside dir, by its text: relative, and not climbing out.
func linkStaysIn(dir, target, linkname string) bool {
	if linkname == "" || filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return false
	}
	rel, err := filepath.Rel(dir, filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname)))
	return err == nil && filepath.IsLocal(rel)
}

// noLinksBetween checks that no directory from root down to dir is a symlink.
func noLinksBetween(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return err
	}
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the archive writes through the symlink %s; not unpacking it", p)
		}
	}
	return nil
}

// packToTemp packs into a temporary file, for an upload with a known size.
// The caller removes the file.
func packToTemp(dir string, files []string) (*os.File, int, int64, error) {
	f, err := os.CreateTemp("", "cloudex-upload-*.tar.gz")
	if err != nil {
		return nil, 0, 0, err
	}
	n, size, err := packDir(f, dir, files)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, 0, err
	}
	return f, n, size, nil
}
