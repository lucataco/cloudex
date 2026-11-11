package node

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Review H1: a machine's root can replace the commands the agent runs in it
// (the ready check, the session scan and kill). Run must neither keep what
// such a command prints without limit nor wait for one that never ends.
func TestRunStopsAtItsOutputCap(t *testing.T) {
	f, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	out, code, err := d.Run(ctx, "m", "0", []string{"yes"})
	if !errors.Is(err, ErrRunOutput) || out != "" || code != -1 {
		t.Fatalf("got %d bytes, exit %d, %v; want ErrRunOutput", len(out), code, err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cfg.Env) != 1 || f.cfg.Env[0] != RunPath {
		t.Fatalf("env %v; want only %s", f.cfg.Env, RunPath)
	}
}

func TestRunEndsWithItsContext(t *testing.T) {
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, _, err := d.Run(ctx, "m", "0", []string{"hang"})
	if err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("err %v after %v; want an error within the deadline", err, time.Since(t0))
	}
}

func TestRunWithoutADeadlineGetsOne(t *testing.T) {
	old := RunTimeout
	defer func() { RunTimeout = old }()
	RunTimeout = 300 * time.Millisecond
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	t0 := time.Now()
	if _, _, err := d.Run(context.Background(), "m", "0", []string{"hang"}); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(t0))
	}
}

func TestRunStillReturnsAnOrdinaryAnswer(t *testing.T) {
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	out, code, err := d.Run(context.Background(), "m", "0", []string{"echo", "ab", "3"})
	if err != nil || out != "ababab" || code != 3 {
		t.Fatalf("%q %d %v", out, code, err)
	}
}
