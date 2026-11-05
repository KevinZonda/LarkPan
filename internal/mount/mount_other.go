//go:build !linux

package mount

import "fmt"

// Client is a stub on non-Linux platforms.
type Client struct {
	Base  string
	Token string
}

// Mount reports that FUSE mounts require Linux.
func Mount(c *Client, dir string) error {
	return fmt.Errorf("mount requires linux with /dev/fuse")
}
