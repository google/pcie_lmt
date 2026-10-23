// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lanemargintest

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

// schedEyes are the timing eyes of the lanes in the scheduler tests. Scanning until fail, the
// lanes margin 5, 14 and 9 points.
var schedEyes = map[uint32]laneEye{
	0: {tPos: upTo(2), tNeg: upTo(1)},
	1: {tPos: upTo(6), tNeg: upTo(6)},
	2: {tPos: upTo(4), tNeg: upTo(3)},
}

// untestedLane is the lane of the scheduler tests without a test spec.
const untestedLane = 3

// newSchedTest returns a fake LMR that fails the commands that fail reports, and a receiver of
// the lanes of schedEyes, which scan their timing eyes until fail, and untestedLane.
func newSchedTest(t *testing.T, fail func(lane uint32, raw uint16) bool) (*fakeLMR, *receiver) {
	t.Helper()
	f := newFakeLMR(schedEyes)
	if fail != nil {
		f.fail = fail
	}
	f.install(t)
	r := newFakeReceiver(len(schedEyes)+1, false)
	for lane := range schedEyes {
		r.lanes[lane].Tspec = &lmtpb.LinkMargin_TestSpec{StartOffset: proto.Uint32(1), ErrorLimit: 3}
	}
	return f, r
}

// soloTraces returns the commands that each lane of schedEyes writes when margined alone.
func soloTraces(t *testing.T) map[uint32][]string {
	t.Helper()
	traces := map[uint32][]string{}
	for lane := range schedEyes {
		f, r := newSchedTest(t, nil)
		r.rxwg.Add(1)
		if err := r.lanes[lane].MarginLane(); err != nil {
			t.Fatalf("MarginLane() of lane %d failed: %v", lane, err)
		}
		traces[lane] = f.laneTrace(lane)
	}
	return traces
}

// prefixed returns the commands of lane as in fakeLMR.globalTrace.
func prefixed(lane uint32, cmds []string) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, fmt.Sprintf("L%d:%s", lane, c))
	}
	return out
}

// captureOCP streams the OCP artifacts to a temporary file until the test ends. The returned
// func returns the artifacts streamed so far.
func captureOCP(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "ocp_pipe")
	if err != nil {
		t.Fatalf("os.CreateTemp() failed: %v", err)
	}
	origPipe := ocpPipe
	ocpPipe = f
	t.Cleanup(func() {
		ocpPipe = origPipe
		f.Close()
	})
	return func() string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("os.ReadFile(%q) failed: %v", f.Name(), err)
		}
		return string(data)
	}
}

// TestMarginLanes checks that both the parallel and the round-robin margining keep the commands
// of each lane as when the lane is margined alone, and skip the lane without a test spec.
func TestMarginLanes(t *testing.T) {
	solo := soloTraces(t)
	for _, parallel := range []bool{true, false} {
		t.Run(fmt.Sprintf("parallel=%t", parallel), func(t *testing.T) {
			f, r := newSchedTest(t, nil)
			r.parallel = parallel

			r.marginLanes()

			for lane := range schedEyes {
				if got := f.laneTrace(lane); !slices.Equal(got, solo[lane]) {
					t.Errorf("marginLanes() wrote %q to lane %d, want %q", got, lane, solo[lane])
				}
				if !r.lanes[lane].Pass || r.lanes[lane].lane == nil {
					t.Errorf("marginLanes() lane %d Pass = %t, gathered = %t, want both true",
						lane, r.lanes[lane].Pass, r.lanes[lane].lane != nil)
				}
			}
			if got := f.laneTrace(untestedLane); len(got) != 0 || r.lanes[untestedLane].lane != nil {
				t.Errorf("marginLanes() wrote %q to lane %d without a test spec, gathered = %t, want nothing",
					got, untestedLane, r.lanes[untestedLane].lane != nil)
			}
		})
	}
}

// TestRoundRobinOrder checks that the round-robin margining reads the parameters of all the
// lanes first, then margins one point of each lane in turn, while the lanes drop out once done.
func TestRoundRobinOrder(t *testing.T) {
	f, r := newSchedTest(t, nil)
	ocp := captureOCP(t)

	r.marginLanes()

	global := f.globalTrace()
	wantParams := slices.Concat(prefixed(0, paramCmds), prefixed(1, paramCmds), prefixed(2, paramCmds))
	if got := global[:min(len(global), len(wantParams))]; !slices.Equal(got, wantParams) {
		t.Errorf("marginLanes() first wrote %q, want the parameters of all lanes %q", got, wantParams)
	}
	var steps []string
	for _, c := range global {
		if strings.Contains(c, ":T") {
			steps = append(steps, c)
		}
	}
	wantSteps := []string{
		"L0:T+1", "L1:T+1", "L2:T+1", "L0:T-1", "L1:T-1", "L2:T-1",
		"L0:T+2", "L1:T+2", "L2:T+2", "L0:T-2", "L1:T-2", "L2:T-2",
		"L0:T+3", "L1:T+3", "L2:T+3", // Lane 0 is done.
		"L1:T-3", "L2:T-3", "L1:T+4", "L2:T+4", "L1:T-4", "L2:T-4",
		"L1:T+5", "L2:T+5", // Lane 2 is done.
		"L1:T-5", "L1:T+6", "L1:T-6", "L1:T+7", "L1:T-7",
	}
	if !slices.Equal(steps, wantSteps) {
		t.Errorf("marginLanes() margined %q, want %q", steps, wantSteps)
	}
	out := ocp()
	lastParams, firstStep := strings.LastIndex(out, "Lane-Parameters"), strings.Index(out, "Step-Status")
	if lastParams < 0 || firstStep < 0 || lastParams > firstStep {
		t.Errorf("marginLanes() streamed the last Lane-Parameters at %d and the first Step-Status at %d, want all Lane-Parameters first",
			lastParams, firstStep)
	}
}

// TestMarginLanesLaneError checks that a lane failing early streams an OCP error, and that the
// other lanes are margined as when alone.
func TestMarginLanesLaneError(t *testing.T) {
	solo := soloTraces(t)
	testCases := []struct {
		name string
		lane uint32 // The failing lane.
		fail uint16 // The command that the lane fails.
		// vspec is the voltage test spec of the failing lane, or nil.
		vspec     *lmtpb.LinkMargin_TestSpec
		wantTrace []string // The commands written to the failing lane.
		wantMsg   string
	}{
		{
			name:      "lane parameters fail",
			lane:      1,
			fail:      rptNumTimingStepsCmd(),
			wantTrace: paramCmds[:5],
		},
		{
			name:      "error limit fails mid-way",
			lane:      0,
			fail:      setErrorLimitCmd(4),
			vspec:     &lmtpb.LinkMargin_TestSpec{TargetOffset: proto.Uint32(1), ErrorLimit: 4},
			wantTrace: slices.Concat(solo[0], []string{"ErrorLimit=4"}),
			wantMsg:   "LMR command failed",
		},
	}
	for _, parallel := range []bool{true, false} {
		for _, tc := range testCases {
			t.Run(fmt.Sprintf("parallel=%t/%s", parallel, tc.name), func(t *testing.T) {
				f, r := newSchedTest(t, func(lane uint32, raw uint16) bool { return lane == tc.lane && raw == tc.fail })
				r.parallel = parallel
				r.lanes[tc.lane].Vspec = tc.vspec
				ocp := captureOCP(t)

				r.marginLanes()

				for lane := range schedEyes {
					want := solo[lane]
					if lane == tc.lane {
						want = tc.wantTrace
					}
					if got := f.laneTrace(lane); !slices.Equal(got, want) {
						t.Errorf("marginLanes() wrote %q to lane %d, want %q", got, lane, want)
					}
				}
				if got := strings.Count(ocp(), fmt.Sprintf("Lane %d failed early", tc.lane)); got != 1 {
					t.Errorf("marginLanes() streamed %d lane %d errors, want 1", got, tc.lane)
				}
				ln := r.lanes[tc.lane]
				if ln.lane != nil || !strings.Contains(ln.msg, tc.wantMsg) {
					t.Errorf("marginLanes() lane %d gathered = %t, message = %q, want not gathered, message containing %q",
						tc.lane, ln.lane != nil, ln.msg, tc.wantMsg)
				}
			})
		}
	}
}

// panicHook panics after the margin point number at of lane.
type panicHook struct {
	lane uint32
	at   int
	n    *int
}

func (panicHook) Name() string { return "panicHook" }

func (h panicHook) PostMargin(ln *Lane, _ *aspect, _ *lmtpb.LinkMargin_Lane_MarginPoint, _ float64, _ time.Duration) error {
	if ln.laneNumber != h.lane {
		return nil
	}
	if *h.n++; *h.n == h.at {
		panic("hook panic")
	}
	return nil
}

// TestRoundRobinPanicStopsLanes checks that a panic in a lane propagates out of the round-robin
// margining, and that the other lanes stop at once, without margining any further point and
// without leaking their coroutines.
func TestRoundRobinPanicStopsLanes(t *testing.T) {
	f, r := newSchedTest(t, nil)
	r.marginHooks = append(r.marginHooks, panicHook{lane: 1, at: 2, n: new(int)})
	before := runtime.NumGoroutine()

	func() {
		defer func() {
			if p := recover(); p != "hook panic" {
				t.Errorf("marginLanes() panicked with %v, want %q", p, "hook panic")
			}
		}()
		r.marginLanes()
	}()

	global := f.globalTrace()
	// The hook runs after the step margin command of the point, before its post-margin commands.
	if want := "L1:T-1"; len(global) == 0 || global[len(global)-1] != want {
		t.Errorf("marginLanes() last wrote %q, want %q", global[max(0, len(global)-1)], want)
	}
	for _, ln := range r.lanes {
		if ln.lane != nil {
			t.Errorf("marginLanes() gathered lane %d, want no lane gathered", ln.laneNumber)
		}
	}
	// The coroutines of the stopped lanes have exited.
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Errorf("marginLanes() left %d goroutines, want at most %d", got, before)
	}
}

// lmrMonitor wraps the config space accesses of a fakeLMR. It counts the accesses in flight,
// and logs them in order.
type lmrMonitor struct {
	inFlight, maxInFlight atomic.Int32

	mu  sync.Mutex
	log []lmrAccess
}

// lmrAccess is a config space access to the LMR of a lane.
type lmrAccess struct {
	lane  uint32
	write bool
	raw   uint16 // The written command, or the read status.
}

func (m *lmrMonitor) enter() {
	n := m.inFlight.Add(1)
	for cur := m.maxInFlight.Load(); n > cur && !m.maxInFlight.CompareAndSwap(cur, n); cur = m.maxInFlight.Load() {
	}
	// Widens the window for another access to overlap.
	runtime.Gosched()
}

func (m *lmrMonitor) exit(a lmrAccess) {
	m.mu.Lock()
	m.log = append(m.log, a)
	m.mu.Unlock()
	m.inFlight.Add(-1)
}

// install routes readWord and writeWord through m to the accesses that are installed.
func (m *lmrMonitor) install(t *testing.T) {
	t.Helper()
	read, write := readWord, writeWord
	t.Cleanup(func() { readWord, writeWord = read, write })
	readWord = func(d pci.Dev, addr int32) uint16 {
		m.enter()
		v := read(d, addr)
		m.exit(lmrAccess{lane: laneOf(addr - 2), raw: v})
		return v
	}
	writeWord = func(d pci.Dev, addr int32, raw uint16) {
		m.enter()
		write(d, addr, raw)
		m.exit(lmrAccess{lane: laneOf(addr), write: true, raw: raw})
	}
}

// violations returns where the logged accesses interleave the LMR commands or the margin
// points of two lanes. An LMR command is a No Command write, its reads, the command write and its
// reads, so another lane may only start with a No Command write after a read. A margin point
// runs from its step margin command to its Go to Normal Settings, with no access to another lane.
func (m *lmrMonitor) violations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	open := -1 // The lane of the margin point in progress, or -1.
	for i, a := range m.log {
		name := cmdName(a.raw)
		if open >= 0 && a.lane != uint32(open) {
			out = append(out, fmt.Sprintf("#%d: lane %d accessed during a margin point of lane %d", i, a.lane, open))
		}
		if i > 0 && a.lane != m.log[i-1].lane && (!a.write || name != "NoCmd" || m.log[i-1].write) {
			out = append(out, fmt.Sprintf("#%d: lane %d interrupted an LMR command of lane %d", i, a.lane, m.log[i-1].lane))
		}
		if !a.write {
			continue
		}
		switch {
		case strings.HasPrefix(name, "T") || strings.HasPrefix(name, "V"):
			open = int(a.lane)
		case name == "GoToNormal":
			open = -1
		}
	}
	return out
}

// mutexEyes are the timing and voltage eyes of the lanes in TestRoundRobinMutualExclusion. The
// lanes have unequal eyes, so they margin unequal numbers of points.
var mutexEyes = map[uint32]laneEye{
	0: {tPos: upTo(2), tNeg: upTo(1), vPos: upTo(3), vNeg: upTo(2)},
	1: {tPos: upTo(6), tNeg: upTo(6), vPos: upTo(8), vNeg: upTo(7)},
	2: {tPos: upTo(4), tNeg: upTo(3), vPos: upTo(20), vNeg: upTo(3)},
	3: {tPos: never, tNeg: upTo(9), vPos: upTo(5), vNeg: never},
}

// TestRoundRobinMutualExclusion checks that the round-robin margining never accesses the LMR of
// two lanes at the same time, and never interleaves their LMR commands or margin points, even
// with many goroutines able to run at once.
func TestRoundRobinMutualExclusion(t *testing.T) {
	// Lets the lanes run in parallel if the scheduling allowed it, so that an overlap can show.
	prev := runtime.GOMAXPROCS(max(4, runtime.NumCPU()))
	defer runtime.GOMAXPROCS(prev)
	newFakeLMR(mutexEyes).install(t)
	r := newFakeReceiver(len(mutexEyes), false)
	for _, ln := range r.lanes {
		ln.Tspec = &lmtpb.LinkMargin_TestSpec{StartOffset: proto.Uint32(1), ErrorLimit: 3}
		ln.Vspec = &lmtpb.LinkMargin_TestSpec{EyeSize: proto.Float32(0.5), ErrorLimit: 5}
	}
	m := new(lmrMonitor)
	m.install(t)

	r.marginLanes()

	if got := m.maxInFlight.Load(); got != 1 {
		t.Errorf("marginLanes() had up to %d LMR accesses in flight, want 1", got)
	}
	if v := m.violations(); len(v) > 0 {
		t.Errorf("marginLanes() interleaved the lanes (%d times), first: %s", len(v), v[0])
	}
	lanes := map[uint32]bool{}
	for _, a := range m.log {
		lanes[a.lane] = true
	}
	if len(lanes) != len(mutexEyes) {
		t.Errorf("marginLanes() accessed lanes %v, want all %d lanes", lanes, len(mutexEyes))
	}
}

// TestParallelOverlapDetected checks that lmrMonitor detects the overlapping accesses of the
// parallel margining, as a control of TestRoundRobinMutualExclusion. The overlap depends on the
// goroutine scheduling, so it only logs the result.
func TestParallelOverlapDetected(t *testing.T) {
	// Lets the parallel lanes run at the same time, so that their accesses can overlap.
	prev := runtime.GOMAXPROCS(max(4, runtime.NumCPU()))
	defer runtime.GOMAXPROCS(prev)
	_, r := newSchedTest(t, nil)
	r.parallel = true
	m := new(lmrMonitor)
	m.install(t)

	r.marginLanes()

	t.Logf("parallel: max LMR accesses in flight = %d, interleavings = %d", m.maxInFlight.Load(), len(m.violations()))
}
