// Package watch turns host readings and the live jobs' time deltas into a memory level and stop
// lists. It reads no clock and does no I/O, so every sampler advances one shared Detector.
package watch

import (
	"slices"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
)

// The detector's defaults and what each rests on; calibration/README.md has the numbers. The points
// were chosen on calibration/records, the gate is a round value, the rest are not calibrated.
const (
	// Chosen on the records: 1 of 339 full-speed samples is held by it, none of 74 control samples.
	DefaultYellowPoints = 20
	// Chosen on the records: none of 947 full-speed or slowing samples reaches it.
	DefaultRedPoints = 40
	// Not calibrated: red needs danger held for tens of seconds (about 20 s); the records allow 3 to 15.
	DefaultRedSamples = 10
	// Chosen: the records give the same levels at 1000, 3000 and 10 000.
	DefaultGatePagesPerS = 1000
	// Not calibrated: a round value; no record has a floor sample.
	DefaultFloorMiB = 2048
	// Not calibrated: ramps turn green 3 to 58 s after a stop by the detector's own clearing.
	DefaultCalmS     = 30
	DefaultPostKillS = 15
	DefaultSpacingS  = 3
)

// minWorkShare is the part of a period a job's user, system and page-wait time must reach to count
// toward E: an idle polling job is mostly system time and says nothing about memory. Not calibrated.
const minWorkShare = 0.10

const (
	clearSamples   = 15                      // samples in a row below yellow_points that clear yellow
	floorGrowthMiB = 256                     // swap growth over the floor window that makes floor an edge
	windowNS       = int64(20 * time.Second) // the floor's growth window
)

// Config is the [watch] table: the thresholds of the levels, in points of excess, samples, pages
// per second and MiB, the waits in seconds, and the mode.
type Config struct {
	YellowPoints, RedPoints, RedSamples, GatePagesPerS float64
	FloorMiB                                           int64
	CalmS, PostKillS, SpacingS                         float64
	Observe, SampleLog                                 bool
}

// Defaults is the configuration without a [watch] table.
func Defaults() Config {
	return Config{YellowPoints: DefaultYellowPoints, RedPoints: DefaultRedPoints,
		RedSamples: DefaultRedSamples, GatePagesPerS: DefaultGatePagesPerS, FloorMiB: DefaultFloorMiB,
		CalmS: DefaultCalmS, PostKillS: DefaultPostKillS, SpacingS: DefaultSpacingS}
}

// Job is one live hold as its run last wrote it: its tree's footprint and the time deltas since
// its run's previous tick, and its label's normal S, which Learned says the run log gave: a job
// without it is neither counted in E nor stopped by a red round.
type Job struct {
	Hold         string  `json:"hold"`
	Label        string  `json:"label"`
	ArrivalNS    int64   `json:"arrival_ns"`
	NeedMiB      int64   `json:"need_mib"`
	FootprintMiB int64   `json:"footprint_mib"`
	DUserNS      int64   `json:"d_user_ns"`
	DSysNS       int64   `json:"d_sys_ns"`
	DPageWaitNS  int64   `json:"d_page_wait_ns"`
	NormalShare  float64 `json:"normal_share"`
	Learned      bool    `json:"learned"`
	Victim       bool    `json:"victim"`
}

// Sample is one machine sample: the host readings, the live jobs, the admission room in MiB and
// the shortage, how far the headroom is under the reserve (0 when it is not).
type Sample struct {
	Host     memsample.HostReading `json:"host"`
	Jobs     []Job                 `json:"jobs"`
	RoomMiB  int64                 `json:"room_mib"`
	ShortMiB int64                 `json:"short_mib"`
}

// Level is the machine's memory level. Admission waits at Yellow and above; Unknown is no
// reading, which never counts toward calm; only the floor can still stop something.
type Level int

const (
	Green Level = iota
	Unknown
	Yellow
	Red
	Floor
)

// String is the level's name; a value read from a damaged state is "unknown".
func (l Level) String() string {
	if names := [...]string{"green", "unknown", "yellow", "red", "floor"}; l >= 0 && int(l) < len(names) {
		return names[l]
	}
	return "unknown"
}

// Rates are the paging counters' rates per second over the last period.
type Rates struct {
	DecompS    float64 `json:"decomp_s"`
	SwapInsS   float64 `json:"swapins_s"`
	PSISomeUSS float64 `json:"psi_some_us_s"`
	HasPSI     bool    `json:"has_psi"`
}

// Point is what the detector keeps of one sample: when, the swap used, and whether its excess
// reached yellow_points with the gate open.
type Point struct {
	AtNS        int64 `json:"at_ns"`
	SwapUsedMiB int64 `json:"swap_used_mib"`
	Hot         bool  `json:"hot"`
}

// Detector is the machine's watch state between samples. Its times are HostReading.WallNS.
type Detector struct {
	Window          []Point  `json:"window"` // the samples of the last 20 s, oldest first
	Rates           Rates    `json:"rates"`
	Excess          float64  `json:"excess"`
	Gate            bool     `json:"gate"`
	Bucket          float64  `json:"bucket"`
	Level           Level    `json:"level"`
	Known           Level    `json:"known"`  // the last level that was not unknown
	Yellow          bool     `json:"yellow"` // latched until clearSamples in a row below yellow
	Below           int      `json:"below"`
	YellowClearedNS int64    `json:"yellow_cleared_ns"`
	CalmSinceNS     int64    `json:"calm_since_ns"` // 0 while the level is not green
	LastRoundNS     int64    `json:"last_round_ns"`
	Observed        bool     `json:"observed"`    // the last round was written in observe mode
	RoundFloor      bool     `json:"round_floor"` // the last round was a floor round
	LastAdmitNS     int64    `json:"last_admit_ns"`
	Stop            []string `json:"stop"` // the last stop round's holds
}

// Advance moves the detector from sample prev to cur and returns the level. A pair it cannot
// compare (memsample.Gap, a counter that went back) is Unknown and keeps the bucket and latch,
// unless the floor holds: it needs no rate. A sleep or a reboot between them empties the bucket.
func (d *Detector) Advance(prev, cur Sample, c Config) Level {
	was, now := d.Known, cur.Host.WallNS
	r, ok := rates(prev.Host, cur.Host)
	d.Rates, d.Excess, d.Gate = r, 0, false
	if ok {
		d.Excess = Excess(cur.Jobs, cur.Host.WallNS-prev.Host.WallNS)
		d.Gate = r.DecompS+r.SwapInsS >= c.GatePagesPerS || r.PSISomeUSS > 0
	}
	d.Window = slices.DeleteFunc(d.Window, func(p Point) bool { return p.AtNS > now || now-p.AtNS > windowNS })
	d.Window = append(d.Window, Point{AtNS: now, SwapUsedMiB: cur.Host.SwapUsedMiB, Hot: d.Gate && d.Excess >= c.YellowPoints})
	hot, low := 0, cur.Host.SwapUsedMiB
	for i, p := range d.Window {
		if p.Hot && i >= len(d.Window)-3 {
			hot++
		}
		low = min(low, p.SwapUsedMiB)
	}
	h := cur.Host
	floor := h.WallNS > 0 && h.VMVolumeFreeMiB+h.SwapFreeMiB+h.MemAvailableMiB < c.FloorMiB && h.SwapUsedMiB-low >= floorGrowthMiB
	if !ok { // the floor needs no rate: a real reading still reaches it
		d.Level, d.CalmSinceNS = Unknown, 0
		if prev.Host.WallNS > 0 && h.WallNS > 0 && memsample.Slept(prev.Host, h) {
			d.Bucket = 0 // red needs its samples after a sleep or a reboot, not before it
		}
		if floor {
			d.Level, d.Known = Floor, Floor
		}
		return d.Level
	}
	if d.Gate && d.Excess >= c.RedPoints {
		d.Bucket = min(d.Bucket+1, c.RedSamples) // saturated, so red ends with the danger
	} else {
		d.Bucket = max(0, d.Bucket-0.5)
	}
	switch {
	case hot >= 2:
		d.Yellow, d.Below = true, 0
	case d.Excess >= c.YellowPoints:
		d.Below = 0
	case d.Yellow:
		d.Below++
		d.Yellow = d.Below < clearSamples
	}
	switch {
	case floor:
		d.Level = Floor
	case d.Bucket >= c.RedSamples:
		d.Level = Red
	case d.Yellow:
		d.Level = Yellow
	default:
		d.Level = Green
	}
	d.Known = d.Level
	if d.Level != Green {
		d.CalmSinceNS = 0
	} else if d.CalmSinceNS == 0 {
		d.CalmSinceNS = now
	}
	if d.Level == Green && was >= Yellow {
		d.YellowClearedNS = now
	}
	return d.Level
}

// rates are the counters' rates from prev to cur; ok is false when the two cannot be compared.
func rates(prev, cur memsample.HostReading) (r Rates, ok bool) {
	s := float64(cur.WallNS-prev.WallNS) / 1e9
	if prev.WallNS == 0 || memsample.Gap(prev, cur) || s <= 0 || cur.Decomp < prev.Decomp ||
		cur.SwapIns < prev.SwapIns || prev.HasPSI && cur.HasPSI && cur.PSISomeUS < prev.PSISomeUS {
		return Rates{}, false
	}
	r = Rates{DecompS: float64(cur.Decomp-prev.Decomp) / s, SwapInsS: float64(cur.SwapIns-prev.SwapIns) / s}
	if prev.HasPSI && cur.HasPSI {
		r.PSISomeUSS, r.HasPSI = float64(cur.PSISomeUS-prev.PSISomeUS)/s, true
	}
	return r, true
}

// Share is the job's S over the period: the part of its time spent in the system or waiting for
// pages. ok is false when the job reported no time.
func Share(j Job) (s float64, ok bool) {
	total := j.DUserNS + j.DSysNS + j.DPageWaitNS
	if total <= 0 {
		return 0, false
	}
	return float64(j.DSysNS+j.DPageWaitNS) / float64(total), true
}

// Excess is the stall excess E in points: the mean of max(0, S − normal) over the learned jobs that
// worked for at least minWorkShare of the period of periodNS ns, weighted by their time; 0 when
// none did.
func Excess(jobs []Job, periodNS int64) float64 {
	var sum, weight float64
	for _, j := range jobs {
		w := float64(j.DUserNS + j.DSysNS + j.DPageWaitNS)
		if s, ok := Share(j); ok && j.Learned && w >= minWorkShare*float64(periodNS) {
			sum += w * max(0, s-j.NormalShare)
			weight += w
		}
	}
	if weight == 0 {
		return 0
	}
	return 100 * sum / weight
}
