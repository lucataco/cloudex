package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newRunCmd(opts *options) *cobra.Command {
	var name, size, from, dir, workdir string
	idle, timeout := 30*time.Minute, time.Duration(0)
	var envPairs, envFiles, outs []string
	var noUpload, keep, keepOnFailure bool
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command>",
		Short: "Run a command in a new sandbox with this directory's files, then delete it",
		Long: "Start a sandbox, copy this directory into it, run a command there with its\n" +
			"output coming back as it's printed, copy back what --out names, and delete the\n" +
			"sandbox. Ends with the command's exit code, for scripts and CI.\n\n" +
			"In a git repository, the files git would commit are copied: tracked ones and\n" +
			"new ones it doesn't ignore, but not .git. Elsewhere, everything under --dir.\n" +
			"They go to a directory of the same name in the home directory, where the\n" +
			"command runs. Uploads are limited to 100 MB compressed.\n\n" +
			"--from starts from a named save, or a fork of a sandbox, instead of the image:\n" +
			"set one up with your tools and dependencies, save it (cloudex save), and each\n" +
			"run starts from it with the latest files copied over.\n\n" +
			"The command runs in the background in the sandbox, so a dropped connection\n" +
			"doesn't stop it, and has no time limit unless --timeout sets one. Ctrl-C stops\n" +
			"it and deletes the sandbox. --keep keeps the sandbox, and --keep-on-failure\n" +
			"keeps it when the command fails or is stopped, to look around with cloudex ssh.\n\n" +
			"A directory copied back by --out leaves out .git directories, hard links,\n" +
			"devices, symlinks that point outside it, and files the sandbox user can't read;\n" +
			"the summary on standard error counts what was left out, but not unreadable files.",
		Example: "  cloudex run -- npm test\n" +
			"  cloudex run --from node-base --out dist -- 'npm ci && npm run build'\n" +
			"  cloudex run --env CI=1 --keep-on-failure -- make check",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() > 0 {
				return errors.New("put the command after --, and cloudex's own flags before it")
			}
			_, command := splitAtDash(cmd, args)
			if command == nil {
				command = args
			}
			if len(command) == 0 {
				return errors.New("no command given after --")
			}
			if from != "" && (len(envFiles) > 0) {
				return errors.New("--env-file doesn't go with --from; use --env")
			}
			r := &runner{cmd: cmd, opts: opts, line: commandLine(command), keep: keep, keepOnFailure: keepOnFailure}
			return r.run(runSpec{
				name: name, size: size, from: from, dir: dir, workdir: workdir, idle: idle, timeout: timeout,
				envPairs: envPairs, envFiles: envFiles, outs: outs, upload: !noUpload,
				idleSet: cmd.Flags().Changed("idle"),
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "start from this named save, or a fork of this sandbox")
	cmd.Flags().StringVar(&name, "name", "", "a name for the sandbox (default: none)")
	cmd.Flags().StringVar(&size, "size", "", "standard-1, standard-2 (default), standard-3 or standard-4")
	cmd.Flags().Var(durationFlag{&idle}, "idle", "stop the sandbox after this long without activity, if it's kept")
	cmd.Flags().Var(durationFlag{&timeout}, "timeout", "stop the command after this long, such as 30m (default: no limit)")
	cmd.Flags().StringVar(&dir, "dir", ".", "the directory to copy into the sandbox")
	cmd.Flags().StringVar(&workdir, "workdir", "", "where it goes in the sandbox, and the command runs (default: the directory's name, in the home directory)")
	cmd.Flags().BoolVar(&noUpload, "no-upload", false, "don't copy any files; run in the home directory unless --workdir says")
	cmd.Flags().StringArrayVar(&outs, "out", nil, "a file or directory to copy back after, relative to the workdir; it lands at the same place under --dir (repeatable)")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "NAME=VALUE: an environment variable (repeatable)")
	cmd.Flags().StringArrayVar(&envFiles, "env-file", nil, "read NAME=VALUE lines from this file (repeatable)")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox after, running")
	cmd.Flags().BoolVar(&keepOnFailure, "keep-on-failure", false, "keep the sandbox when the command fails")
	return cmd
}

type runSpec struct {
	name, size, from, dir, workdir string
	idle, timeout                  time.Duration
	idleSet                        bool
	envPairs, envFiles, outs       []string
	upload                         bool
}

type runner struct {
	cmd                 *cobra.Command
	opts                *options
	client              *api.Client
	line                string
	keep, keepOnFailure bool
}

func (r *runner) say(format string, a ...any) {
	fmt.Fprintf(r.cmd.ErrOrStderr(), "cloudex: "+format+"\n", a...)
}

func (r *runner) run(spec runSpec) (err error) {
	_, client, err := r.opts.load()
	if err != nil {
		return err
	}
	r.client = client
	env, err := envSettings(spec.envPairs, spec.envFiles)
	if err != nil {
		return err
	}
	if spec.timeout < 0 || spec.timeout > 0 && spec.timeout < time.Second {
		return errors.New("--timeout must be at least 1s")
	}
	for _, o := range spec.outs {
		if o == "" || path.IsAbs(o) || !filepath.IsLocal(filepath.FromSlash(o)) || path.Clean(o) == "." || hasGitDir(filepath.FromSlash(path.Clean(o))) {
			return fmt.Errorf("--out %s: give a path inside the workdir, such as dist or build/report.xml", o)
		}
	}
	dir, err := filepath.Abs(spec.dir)
	if err != nil {
		return err
	}
	workdir := spec.workdir
	if workdir == "" && spec.upload {
		workdir = filepath.Base(dir)
	}
	if workdir == "" {
		workdir = "."
	}

	// Pack first, so that nothing starts when the files can't go.
	var archive *os.File
	var entries int
	var size int64
	if spec.upload {
		files, inGit, err := gitFiles(dir)
		if err != nil {
			return err
		}
		if !inGit {
			files = nil
		}
		if inGit && len(files) == 0 {
			return fmt.Errorf("git lists no files in %s to copy; use --no-upload to run without any", dir)
		}
		archive, entries, size, err = packToTemp(dir, files)
		if err != nil {
			return err
		}
		defer os.Remove(archive.Name())
		defer archive.Close()
		if err := checkUploadSize(archive, "The upload from "+dir); err != nil {
			return err
		}
	}

	// From here on, Ctrl-C stops the command, and the sandbox is deleted.
	ctx, stop := signal.NotifyContext(r.cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t0 := time.Now()
	// Starting goes on through a Ctrl-C, so that the sandbox can be deleted.
	box, err := r.start(context.WithoutCancel(ctx), spec, env)
	if box != nil {
		defer func() { r.finish(box, err) }()
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	r.say("%s is running (%s), after %s", box.Label(), box.Size, time.Since(t0).Round(100*time.Millisecond))
	if box.Notice != nil && *box.Notice != "" {
		r.say("%s", *box.Notice)
	}
	id := box.ID
	if archive != nil {
		info, err := archive.Stat()
		if err != nil {
			return err
		}
		res, err := client.UploadArchive(ctx, id, workdir, false, archive)
		if err != nil {
			return fmt.Errorf("copying the files: %w", err)
		}
		workdir = res.Path
		r.say("copied %s (%s, %s compressed) to %s", plural(int64(entries), "file and directory"), humanBytes(size), humanBytes(info.Size()), res.Path)
	}

	c, err := client.StartCommand(ctx, id, api.StartCommandRequest{Cmd: r.line, Cwd: workdir, TimeoutSec: int(spec.timeout.Seconds())})
	if err != nil {
		return err
	}
	ended, err := r.follow(ctx, id, c)
	if err != nil {
		return err
	}
	if ended.State == "lost" {
		return errors.New("the sandbox stopped while the command ran")
	}
	r.say("the command ended (%s) after %s", commandState(ended), (time.Duration(deref64(ended.EndedAt)-ended.StartedAt) * time.Millisecond).Round(100*time.Millisecond))

	var missing []string
	for _, o := range spec.outs {
		remote := path.Join(workdir, filepath.ToSlash(o))
		local := filepath.Join(dir, filepath.FromSlash(o))
		if err := r.fetch(context.WithoutCancel(ctx), id, remote, local); err != nil {
			r.say("couldn't copy back %s: %v", o, err)
			missing = append(missing, o)
		}
	}
	if ended.State == "killed" && ctx.Err() != nil {
		return exitError{130} // stopped with Ctrl-C
	}
	if err := commandExit(ended); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("the command succeeded, but %s couldn't be copied back", strings.Join(missing, ", "))
	}
	return nil
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// start creates the sandbox, or starts it from a save or a fork.
func (r *runner) start(ctx context.Context, spec runSpec, env map[string]string) (*api.Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, saveTimeout)
	defer cancel()
	idle := (*int)(nil)
	if spec.idleSet {
		minutes, err := idleMinutes(spec.idle)
		if err != nil {
			return nil, err
		}
		idle = &minutes
	}
	if spec.from == "" {
		req := api.CreateRequest{Name: spec.name, Size: spec.size, IdleMinutes: idle}
		if len(env) > 0 {
			req.Env = env
		}
		r.say("starting a sandbox")
		s, err := r.client.Create(ctx, req)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	ref, isSave, err := fromSave(ctx, r.client, spec.from)
	if err != nil {
		return nil, err
	}
	req := api.ForkRequest{Name: spec.name, Count: 1, Size: spec.size, IdleMinutes: idle}
	var list []api.Sandbox
	if isSave {
		r.say("starting a sandbox from the save %s", ref)
		list, err = r.client.StartFromSave(ctx, ref, req)
	} else {
		r.say("starting a fork of %s", ref)
		list, err = r.client.Fork(ctx, ref, req)
	}
	if err != nil {
		return nil, err
	}
	if len(list) != 1 {
		return nil, fmt.Errorf("the API started %d sandboxes, not 1", len(list))
	}
	s := &list[0]
	if s.State != "running" {
		return s, fmt.Errorf("%s didn't start: %s", s.Label(), deref(s.Error))
	}
	if len(env) > 0 {
		change := api.SettingsChange{Env: map[string]*string{}}
		for k, v := range env {
			change.Env[k] = &v
		}
		if _, err := r.client.UpdateSettings(ctx, s.ID, change); err != nil {
			return s, fmt.Errorf("setting the environment variables: %w", err)
		}
	}
	return s, nil
}

// follow prints the command's output until it ends. The first Ctrl-C stops
// it; the output that follows still prints.
func (r *runner) follow(ctx context.Context, id string, c *api.Command) (*api.Command, error) {
	out, errOut := r.cmd.OutOrStdout(), r.cmd.ErrOrStderr()
	var after int64
	ended, err := followCommand(ctx, r.client, id, c.ID, &after, true, out, errOut)
	if err == nil || ctx.Err() == nil || r.cmd.Context().Err() != nil {
		return ended, err
	}
	// Ctrl-C: stop the command, then read to its end. Another Ctrl-C quits.
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	r.say("stopping the command; Ctrl-C again to quit and leave the sandbox")
	ctx = context.WithoutCancel(ctx)
	if _, err := r.client.KillCommand(ctx, id, c.ID, "TERM"); err != nil {
		return nil, err
	}
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return followCommand(timeout, r.client, id, c.ID, &after, true, out, errOut)
}

// fetch copies a file or directory out of the sandbox.
func (r *runner) fetch(ctx context.Context, id, remote, local string) error {
	info, err := r.client.Stat(ctx, id, remote, false)
	if err != nil {
		return err
	}
	if info.Type == "dir" {
		body, err := r.client.DownloadArchive(ctx, id, info.Path, false)
		if err != nil {
			return err
		}
		defer body.Close()
		n, skipped, err := unpack(body, local)
		if err != nil {
			return err
		}
		r.say("copied back %s (%s%s)", local, plural(int64(n), "entry"), leftOut(skipped))
		return nil
	}
	msg, err := download(ctx, r.client, id, info.Path, local, false, io.Discard)
	if err == nil {
		r.say("%s", strings.TrimSuffix(strings.Replace(msg, "Copied", "copied back", 1), "."))
	}
	return err
}

// finish deletes the sandbox, unless it's to be kept.
func (r *runner) finish(box *api.Sandbox, err error) {
	failed := err != nil
	if r.keep || r.keepOnFailure && failed {
		r.say("kept %s; connect with: cloudex ssh %s, and delete it with: cloudex rm %s", box.Label(), box.ID, box.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if derr := r.client.Delete(ctx, box.ID); derr != nil {
		r.say("couldn't delete %s: %v; delete it with: cloudex rm %s", box.Label(), derr, box.ID)
		return
	}
	r.say("deleted %s", box.Label())
}
