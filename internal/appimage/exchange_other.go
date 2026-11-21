//go:build !amd64 && !arm64

package appimage

// sysRenameat2 is unknown here; exchange then reports ENOSYS.
const sysRenameat2 = -1
