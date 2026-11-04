package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

// A sandbox reference before the colon of SANDBOX:PATH, by the rules for
// names and ids; anything else (./a:b, C:\x) is a local path.
var remoteRef = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// remotePath splits SANDBOX:PATH, or :PATH for the current sandbox.
func remotePath(s string) (sandbox, p string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	if i > 0 && !remoteRef.MatchString(s[:i]) {
		return "", "", false
	}
	p = s[i+1:]
	if p == "" {
		p = "."
	}
	return s[:i], p, true
}

func newCpCmd(opts *options) *cobra.Command {
	var root, ignored bool
	cmd := &cobra.Command{
		Use:   "cp <from> <to>",
		Short: "Copy files and directories into or out of a sandbox",
		Long: "Copy a file or a directory between this computer and a sandbox. Name a path in\n" +
			"a sandbox as SANDBOX:PATH, or :PATH for the current sandbox; relative paths\n" +
			"there start in the home directory.\n\n" +
			"Like cp -r: when <to> is a directory that exists, or ends with /, what you copy\n" +
			"goes inside it; otherwise it's created as <to>. A file keeps its permissions,\n" +
			"and a directory goes as a tar.gz. A <from> of - reads a file from standard\n" +
			"input, and a <to> of - writes one to standard output. Uploads are limited to\n" +
			"100 MB at a time (Cloudflare's limit); for more, use rsync over cloudex ssh-config.\n\n" +
			"A directory copied out of a sandbox leaves out .git directories, hard links,\n" +
			"devices, symlinks that point outside it, and files the sandbox user can't read\n" +
			"(--root reads them). The summary on standard error counts what was left out,\n" +
			"but not the files that couldn't be read.",
		Example: "  cloudex cp report.csv :data/\n" +
			"  cloudex cp ./site my-box:/srv/site\n" +
			"  cloudex cp :app/dist ./dist\n" +
			"  cloudex cp my-box:/var/log/app.log -",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			fromBox, fromPath, fromRemote := remotePath(args[0])
			toBox, toPath, toRemote := remotePath(args[1])
			switch {
			case fromRemote && toRemote:
				return errors.New("copy between this computer and a sandbox: one side must be local")
			case !fromRemote && !toRemote:
				return errors.New("name a path in a sandbox as SANDBOX:PATH, or :PATH for the current one")
			}
			box := fromBox + toBox
			r, err := ref(cfg, box)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var msg string
			if toRemote {
				msg, err = upload(ctx, client, r, args[0], toPath, args[1], root, cmd.InOrStdin())
			} else {
				msg, err = download(ctx, client, r, fromPath, args[1], root, cmd.OutOrStdout())
			}
			if err != nil {
				return err
			}
			if msg != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), msg)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "read and write in the sandbox as root")
	// Habit from cp, scp and rsync: directories always copy whole.
	cmd.Flags().BoolVarP(&ignored, "recursive", "r", false, "accepted and ignored")
	cmd.Flags().BoolVarP(&ignored, "archive", "a", false, "accepted and ignored")
	cmd.Flags().BoolVarP(&ignored, "R", "R", false, "accepted and ignored")
	for _, name := range []string{"recursive", "archive", "R"} {
		cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// remoteDest resolves the remote destination: inside it when it ends with / or is
// a directory, with the source's name.
func remoteDest(ctx context.Context, client *api.Client, r, dest, name string, root bool) (string, error) {
	if strings.HasSuffix(dest, "/") || dest == "." || dest == "~" {
		return path.Join(dest, name), nil
	}
	info, err := client.Stat(ctx, r, dest, root)
	var apiErr *api.Error
	switch {
	case err == nil && info.Type == "dir":
		return path.Join(dest, name), nil
	case err == nil || errors.As(err, &apiErr) && apiErr.Status == 404:
		return dest, nil
	}
	return "", err
}

func upload(ctx context.Context, client *api.Client, r, src, dest, shown string, root bool, stdin io.Reader) (string, error) {
	if src == "-" {
		if strings.HasSuffix(dest, "/") {
			return "", errors.New("give the file's name in the sandbox when copying from standard input")
		}
		fi, err := client.WriteFile(ctx, r, dest, root, "", stdin)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Wrote %s (%s) in %s.", fi.Path, humanBytes(fi.Size), r), nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	dest, err = remoteDest(ctx, client, r, dest, filepath.Base(abs), root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if info.Size() > uploadLimit {
			return "", fmt.Errorf("%s is %s, over Cloudflare's 100 MB limit for one upload; use rsync over cloudex ssh-config", src, humanBytes(info.Size()))
		}
		fi, err := client.WriteFile(ctx, r, dest, root, fmt.Sprintf("%o", info.Mode().Perm()), f)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Copied %s to %s:%s (%s).", src, r, fi.Path, humanBytes(fi.Size)), nil
	}
	f, n, size, err := packToTemp(src, nil)
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := checkUploadSize(f, src); err != nil {
		return "", err
	}
	res, err := client.UploadArchive(ctx, r, dest, root, f)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Copied %s to %s:%s (%s, %s).", src, r, res.Path, plural(int64(n), "entry"), humanBytes(size)), nil
}

func checkUploadSize(f *os.File, what string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > uploadLimit {
		return fmt.Errorf("%s comes to %s compressed, over Cloudflare's 100 MB limit for one upload; copy less at a time, or use rsync over cloudex ssh-config", what, humanBytes(info.Size()))
	}
	return nil
}

func download(ctx context.Context, client *api.Client, r, src, dest string, root bool, stdout io.Writer) (string, error) {
	info, err := client.Stat(ctx, r, src, root)
	if err != nil {
		return "", err
	}
	// The local name comes from what was asked for, never from what the
	// sandbox answers, which could name another file (a symlink to ~/.zshrc).
	name := path.Base(path.Clean(src))
	if name == "." || name == "~" || name == "/" || name == ".." {
		name = path.Base(info.Path)
	}
	if name == "" || name == "." || name == ".." || name == "/" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("can't tell what to call %s here; give the local path in full", src)
	}
	// Follow a symlink, as cp does for one named on the command line.
	for i := 0; info.Type == "symlink" && i < 10; i++ {
		target := info.Target
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(info.Path), target)
		}
		if info, err = client.Stat(ctx, r, target, root); err != nil {
			return "", err
		}
	}
	isDir := info.Type == "dir"
	if dest == "-" {
		if isDir {
			return "", fmt.Errorf("%s is a directory; copy it to a local directory instead of -", info.Path)
		}
		body, err := client.ReadFile(ctx, r, info.Path, root)
		if err != nil {
			return "", err
		}
		defer body.Close()
		_, err = io.Copy(stdout, body)
		return "", err
	}
	if st, err := os.Stat(dest); err == nil && st.IsDir() || strings.HasSuffix(dest, string(filepath.Separator)) || strings.HasSuffix(dest, "/") {
		dest = filepath.Join(dest, name)
	}
	if isDir {
		body, err := client.DownloadArchive(ctx, r, info.Path, root)
		if err != nil {
			return "", err
		}
		defer body.Close()
		n, skipped, err := unpack(body, dest)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Copied %s:%s to %s (%s%s).", r, info.Path, dest, plural(int64(n), "entry"), leftOut(skipped)), nil
	}
	body, err := client.ReadFile(ctx, r, info.Path, root)
	if err != nil {
		return "", err
	}
	defer body.Close()
	mode := fs.FileMode(0o644)
	var m uint32
	if _, err := fmt.Sscanf(info.Mode, "%o", &m); err == nil && info.Type == "file" {
		mode = fs.FileMode(m).Perm()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".cloudex-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return fmt.Sprintf("Copied %s:%s to %s (%s).", r, info.Path, dest, humanBytes(n)), nil
}

// leftOut notes the entries unpack left out.
func leftOut(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; left out %s: .git directories, hard links, devices, and symlinks pointing outside it", plural(int64(n), "entry"))
}
