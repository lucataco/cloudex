package node

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path (made if need be) until unlock:
// forks.json is written by the agent and by a hand-run `cloudex-node forks
// --sweep`, two processes (review F-L2).
func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
