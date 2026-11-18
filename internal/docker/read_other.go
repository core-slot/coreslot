//go:build !unix

package docker

import "time"

// Read is no reading where there is no unix socket to read.
func Read(sockets []string, deadline time.Time) Reading {
	return Reading{Code: "unsupported"}
}
