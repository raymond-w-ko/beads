//go:build unix

package timelog

import "syscall"

// oNoFollow makes the append's open refuse a symlink as the path's last
// component.
const oNoFollow = syscall.O_NOFOLLOW
