//go:build !unix

package coreslot

import (
	"context"
	"os"
	"slices"
)

// Client is an opened budget. Without Unix, none can be opened.
type Client struct{}

// Waiter is a queued request. Without Unix, none exists.
type Waiter struct{}

// Grant is what an admission holds. Without Unix, none exists.
type Grant struct{}

// Open returns ErrUnsupported: a platform without Unix (Windows, Plan 9, WebAssembly) has no
// coreslot budget, since the protocol rests on flock(2) and descriptor inheritance.
func Open(Options) (*Client, error) { return nil, ErrUnsupported }

// The methods below exist so that portable programs compile; no value can reach them, since
// Open returns no Client. Each returns ErrUnsupported where it returns an error, Close included:
// a call that cannot have happened reports the platform rather than a success. Environ returns a
// copy of base, and the others zero values.

// Close returns ErrUnsupported.
func (c *Client) Close() error { return ErrUnsupported }

// Enqueue returns ErrUnsupported.
func (c *Client) Enqueue(context.Context, Request) (*Waiter, error) { return nil, ErrUnsupported }

// Adopt returns ErrUnsupported.
func (c *Client) Adopt(context.Context, Request) (*Grant, error) { return nil, ErrUnsupported }

// Available returns ErrUnsupported.
func (c *Client) Available(string) (int, error) { return 0, ErrUnsupported }

// Snapshot returns ErrUnsupported.
func (c *Client) Snapshot() (Report, error) { return Report{}, ErrUnsupported }

// Poll returns ErrUnsupported.
func (w *Waiter) Poll() (*Grant, error) { return nil, ErrUnsupported }

// Wait returns ErrUnsupported.
func (w *Waiter) Wait(context.Context) (*Grant, error) { return nil, ErrUnsupported }

// Cancel returns ErrUnsupported.
func (w *Waiter) Cancel() error { return ErrUnsupported }

// Granted returns nil.
func (g *Grant) Granted() map[string]int { return nil }

// Clamped returns nil.
func (g *Grant) Clamped() map[string]int { return nil }

// Shortfall returns nil.
func (g *Grant) Shortfall() map[string]int { return nil }

// Unbudgeted returns nil.
func (g *Grant) Unbudgeted() []string { return nil }

// TokenOnly returns false.
func (g *Grant) TokenOnly() bool { return false }

// PassThrough returns false.
func (g *Grant) PassThrough() bool { return false }

// Files returns nil.
func (g *Grant) Files() []*os.File { return nil }

// Env returns nil.
func (g *Grant) Env() []string { return nil }

// Environ returns a copy of base.
func (g *Grant) Environ(base []string) []string { return slices.Clone(base) }

// Close returns ErrUnsupported.
func (g *Grant) Close() error { return ErrUnsupported }
