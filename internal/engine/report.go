package engine

// The inspection types live in a file without a build constraint so that the root package can
// alias them on every platform.

// HolderInfo describes one live hold as a waiter or a snapshot sees it.
type HolderInfo struct {
	Name                string         // the hold's file name
	Label               string         // the holder's label, unescaped: escape it before printing
	PID                 int            // the owner's pid, from the name
	StartNS             int64          // wall clock at admission, from the name
	Grant               map[string]int // resource -> number of tokens held
	Unbudgeted          bool           // a fail-open hold
	UnbudgetedResources []string
	Clamped             map[string]int // resource -> requested min, for clamped resources
	Adopted             bool
}
