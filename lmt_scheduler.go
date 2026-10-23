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

// Lane scheduling, which decides the order of the margin points across the lanes of a receiver.

import (
	"errors"
	"iter"
	"runtime/debug"
	"slices"
	"sync"

	log "github.com/golang/glog"
)

// marginLanes margins the lanes of r that have a test spec. The lanes run in parallel if the
// receiver lane has an independent error sampler, and in turn, one point per lane, otherwise.
func (r *receiver) marginLanes() {
	var lanes []*Lane
	for _, ln := range r.lanes {
		if ln.Vspec != nil || ln.Tspec != nil {
			lanes = append(lanes, ln)
		}
	}
	if r.parallel {
		r.marginInParallel(lanes)
	} else {
		r.marginInTurn(lanes)
	}
}

// marginInParallel margins each lane in its own goroutine.
func (r *receiver) marginInParallel(lanes []*Lane) {
	var wg sync.WaitGroup
	for _, ln := range lanes {
		wg.Add(1)
		// Some retimer cannot handle parameter reading overlapping with margining on another lane.
		r.rxwg.Add(1)
		go func(ln *Lane) {
			defer wg.Done()
			r.marginLane(ln)
		}(ln)
	}
	wg.Wait()
}

// marginLane margins ln, and streams an OCP error artifact if it fails early.
func (r *receiver) marginLane(ln *Lane) {
	if err := ln.MarginLane(); err != nil {
		r.outputLaneError(ln, err)
	}
}

// marginInTurn margins one point of each lane in turn, in the calling goroutine. Margining a
// lane at many consecutive points accumulates its receiver errors, e.g. in the Leaky Bucket of
// a Root Port, which can degrade the link speed. Like margining the lanes one after
// another, it never margins two lanes at the same time: each lane is an iter.Pull coroutine that
// pauses after each margin point, and only the resumed one runs.
func (r *receiver) marginInTurn(lanes []*Lane) {
	// Reads the parameters of all the lanes before margining any.
	turns := make([]*laneTurns, 0, len(lanes))
	for _, ln := range lanes {
		if err := ln.readLaneParameters(); err != nil {
			log.Errorf("%s: Failed to read lane parameters: %v", ln.laneRepr(), err)
			r.outputLaneError(ln, err)
			continue
		}
		lt := &laneTurns{ln: ln}
		var stop func()
		lt.next, stop = iter.Pull(lt.margin)
		defer stop()
		turns = append(turns, lt)
	}
	r.linkwg.Wait()

	for len(turns) > 0 {
		for i := 0; i < len(turns); {
			if _, ok := turns[i].next(); ok {
				i++
				continue
			}
			if err := turns[i].err; err != nil {
				r.outputLaneError(turns[i].ln, err)
			}
			turns = slices.Delete(turns, i, i+1)
		}
	}
}

// laneTurns margins a lane in turns, one point per turn.
type laneTurns struct {
	ln   *Lane
	next func() (struct{}, bool) // Margins the next point; false once the lane is done.
	err  error                   // The error that ended the lane early.
}

// errTurnsStopped unwinds a lane whose turns are stopped before it is done.
var errTurnsStopped = errors.New("lane turns stopped")

// margin margins the aspects of the lane, and yields after each margin point. If the turns are
// stopped, it returns at once without margining any further point.
func (lt *laneTurns) margin(yield func(struct{}) bool) {
	defer func() {
		if p := recover(); p != nil && p != errTurnsStopped {
			// iter.Pull re-panics in the caller of next, which loses the stack of the lane.
			log.Errorf("%s: panic: %v\n%s", lt.ln.laneRepr(), p, debug.Stack())
			panic(p)
		}
	}()
	lt.err = lt.ln.marginAspects(func() {
		if !yield(struct{}{}) {
			panic(errTurnsStopped)
		}
	})
}
