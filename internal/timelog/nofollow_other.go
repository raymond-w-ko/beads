//go:build !unix

package timelog

// oNoFollow is zero where the platform's open has no O_NOFOLLOW (Windows):
// there a symlink as the path's last component is followed.
const oNoFollow = 0
