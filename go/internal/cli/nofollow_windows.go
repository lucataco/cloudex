//go:build windows

package cli

// Windows has no O_NOFOLLOW; unpack checks for symlinks before opening.
const oNoFollow = 0
