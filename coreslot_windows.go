//go:build windows

package coreslot

import (
	"context"
	"os"
)

// Client is an opened budget. On Windows none can be opened.
type Client struct{}

// Waiter is a queued request. On Windows none exists.
type Waiter struct{}

// Grant is what an admission holds. On Windows none exists.
type Grant struct{}

// Open returns ErrUnsupported: Windows has no coreslot budget.
func Open(Options) (*Client, error) { return nil, ErrUnsupported }

// The methods below exist so that portable programs compile; no value can reach them.

func (c *Client) Enqueue(Request) (*Waiter, error)              { return nil, ErrUnsupported }
func (c *Client) Adopt(Request) (*Grant, map[string]int, error) { return nil, nil, ErrUnsupported }
func (c *Client) Free(string) (int, error)                      { return 0, ErrUnsupported }
func (c *Client) Snapshot() (Report, error)                     { return Report{}, ErrUnsupported }
func (w *Waiter) Poll() (*Grant, bool, error)                   { return nil, false, ErrUnsupported }
func (w *Waiter) Wait(context.Context) (*Grant, error)          { return nil, ErrUnsupported }
func (w *Waiter) Cancel() error                                 { return ErrUnsupported }
func (g *Grant) Grant() map[string]int                          { return nil }
func (g *Grant) Clamped() map[string]int                        { return nil }
func (g *Grant) Unbudgeted() []string                           { return nil }
func (g *Grant) TokenOnly() bool                                { return false }
func (g *Grant) Files() []*os.File                              { return nil }
func (g *Grant) Env() []string                                  { return nil }
func (g *Grant) Close() error                                   { return nil }
