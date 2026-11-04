//go:build !windows

package cli

import "syscall"

// oNoFollow makes opening a file fail on a symlink rather than follow it.
const oNoFollow = syscall.O_NOFOLLOW
