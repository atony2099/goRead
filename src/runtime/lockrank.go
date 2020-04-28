// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file records the static ranks of the locks in the runtime. If a lock
// is not given a rank, then it is assumed to be a leaf lock, which means no other
// lock can be acquired while it is held. Therefore, leaf locks do not need to be
// given an explicit rank. We list all of the architecture-independent leaf locks
// for documentation purposes, but don't list any of the architecture-dependent
// locks (which are all leaf locks). debugLock is ignored for ranking, since it is used
// when printing out lock ranking errors.
//
// lockInit(l *mutex, rank int) is used to set the rank of lock before it is used.
// If there is no clear place to initialize a lock, then the rank of a lock can be
// specified during the lock call itself via lockWithrank(l *mutex, rank int).
//
// Besides the static lock ranking (which is a total ordering of the locks), we
// also represent and enforce the actual partial order among the locks in the
// arcs[] array below. That is, if it is possible that lock B can be acquired when
// lock A is the previous acquired lock that is still held, then there should be
// an entry for A in arcs[B][]. We will currently fail not only if the total order
// (the lock ranking) is violated, but also if there is a missing entry in the
// partial order.

package runtime

type lockRank int

// Constants representing the lock rank of the architecture-independent locks in
// the runtime. Locks with lower rank must be taken before locks with higher
// rank.
const (
	lockRankDummy lockRank = iota

	// Locks held above sched
	lockRankScavenge
	lockRankForcegc
	lockRankSweepWaiters
	lockRankAssistQueue
	lockRankCpuprof
	lockRankSweep

	lockRankSched
	lockRankDeadlock
	lockRankPanic
	lockRankAllg
	lockRankAllp
	lockRankPollDesc

	lockRankTimers // Multiple timers locked simultaneously in destroy()
	lockRankItab
	lockRankReflectOffs
	lockRankHchan // Multiple hchans acquired in lock order in syncadjustsudogs()
	lockRankFin
	lockRankNotifyList
	lockRankTraceBuf
	lockRankTraceStrings
	lockRankMspanSpecial
	lockRankProf
	lockRankGcBitsArenas
	lockRankRoot
	lockRankTrace
	lockRankTraceStackTab
	lockRankNetpollInit

	lockRankRwmutexW
	lockRankRwmutexR

	lockRankMcentral
	lockRankSpine
	lockRankStackpool
	lockRankStackLarge
	lockRankDefer
	lockRankSudog

	// Memory-related non-leaf locks
	lockRankWbufSpans
	lockRankMheap
	lockRankMheapSpecial

	// Memory-related leaf locks
	lockRankGlobalAlloc

	// Other leaf locks
	lockRankGFree

	// Leaf locks with no dependencies, so these constants are not actually used anywhere.
	// There are other architecture-dependent leaf locks as well.
	lockRankNewmHandoff
	lockRankDebugPtrmask
	lockRankFaketimeState
	lockRankTicks
	lockRankRaceFini
	lockRankPollCache
	lockRankDebug
)

// lockRankLeafRank is the rank of lock that does not have a declared rank, and hence is
// a leaf lock.
const lockRankLeafRank lockRank = 1000

// lockNames gives the names associated with each of the above ranks
var lockNames = []string{
	lockRankDummy: "",

	lockRankScavenge:     "scavenge",
	lockRankForcegc:      "forcegc",
	lockRankSweepWaiters: "sweepWaiters",
	lockRankAssistQueue:  "assistQueue",
	lockRankCpuprof:      "cpuprof",
	lockRankSweep:        "sweep",

	lockRankSched:    "sched",
	lockRankDeadlock: "deadlock",
	lockRankPanic:    "panic",
	lockRankAllg:     "allg",
	lockRankAllp:     "allp",
	lockRankPollDesc: "pollDesc",

	lockRankTimers:      "timers",
	lockRankItab:        "itab",
	lockRankReflectOffs: "reflectOffs",

	lockRankHchan:         "hchan",
	lockRankFin:           "fin",
	lockRankNotifyList:    "notifyList",
	lockRankTraceBuf:      "traceBuf",
	lockRankTraceStrings:  "traceStrings",
	lockRankMspanSpecial:  "mspanSpecial",
	lockRankProf:          "prof",
	lockRankGcBitsArenas:  "gcBitsArenas",
	lockRankRoot:          "root",
	lockRankTrace:         "trace",
	lockRankTraceStackTab: "traceStackTab",
	lockRankNetpollInit:   "netpollInit",

	lockRankRwmutexW: "rwmutexW",
	lockRankRwmutexR: "rwmutexR",

	lockRankMcentral:   "mcentral",
	lockRankSpine:      "spine",
	lockRankStackpool:  "stackpool",
	lockRankStackLarge: "stackLarge",
	lockRankDefer:      "defer",
	lockRankSudog:      "sudog",

	lockRankWbufSpans:    "wbufSpans",
	lockRankMheap:        "mheap",
	lockRankMheapSpecial: "mheapSpecial",

	lockRankGlobalAlloc: "globalAlloc.mutex",

	lockRankGFree: "gFree",

	lockRankNewmHandoff:   "newmHandoff.lock",
	lockRankDebugPtrmask:  "debugPtrmask.lock",
	lockRankFaketimeState: "faketimeState.lock",
	lockRankTicks:         "ticks.lock",
	lockRankRaceFini:      "raceFiniLock",
	lockRankPollCache:     "pollCache.lock",
	lockRankDebug:         "debugLock",
}

func (rank lockRank) String() string {
	if rank == 0 {
		return "UNKNOWN"
	}
	if rank == lockRankLeafRank {
		return "LEAF"
	}
	return lockNames[rank]
}

// lockPartialOrder is a partial order among the various lock types, listing the immediate
// ordering that has actually been observed in the runtime. Each entry (which
// corresponds to a particular lock rank) specifies the list of locks that can be
// already be held immediately "above" it.
//
// So, for example, the lockRankSched entry shows that all the locks preceding it in
// rank can actually be held. The fin lock shows that only the sched, timers, or
// hchan lock can be held immediately above it when it is acquired.
var lockPartialOrder [][]lockRank = [][]lockRank{
	lockRankDummy:         {},
	lockRankScavenge:      {},
	lockRankForcegc:       {},
	lockRankSweepWaiters:  {},
	lockRankAssistQueue:   {},
	lockRankCpuprof:       {},
	lockRankSweep:         {},
	lockRankSched:         {lockRankScavenge, lockRankForcegc, lockRankSweepWaiters, lockRankAssistQueue, lockRankCpuprof, lockRankSweep},
	lockRankDeadlock:      {lockRankDeadlock},
	lockRankPanic:         {lockRankDeadlock},
	lockRankAllg:          {lockRankSched, lockRankPanic},
	lockRankAllp:          {lockRankSched},
	lockRankPollDesc:      {},
	lockRankTimers:        {lockRankScavenge, lockRankSched, lockRankAllp, lockRankPollDesc, lockRankTimers},
	lockRankItab:          {},
	lockRankReflectOffs:   {lockRankItab},
	lockRankHchan:         {lockRankScavenge, lockRankSweep, lockRankHchan},
	lockRankFin:           {lockRankSched, lockRankAllg, lockRankTimers, lockRankHchan},
	lockRankNotifyList:    {},
	lockRankTraceBuf:      {},
	lockRankTraceStrings:  {lockRankTraceBuf},
	lockRankMspanSpecial:  {lockRankScavenge, lockRankCpuprof, lockRankSched, lockRankAllg, lockRankAllp, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankHchan, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings},
	lockRankProf:          {lockRankScavenge, lockRankAssistQueue, lockRankCpuprof, lockRankSweep, lockRankSched, lockRankAllg, lockRankAllp, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan},
	lockRankGcBitsArenas:  {lockRankScavenge, lockRankAssistQueue, lockRankCpuprof, lockRankSched, lockRankAllg, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan},
	lockRankRoot:          {},
	lockRankTrace:         {lockRankScavenge, lockRankAssistQueue, lockRankSched, lockRankHchan, lockRankTraceBuf, lockRankTraceStrings, lockRankRoot, lockRankSweep},
	lockRankTraceStackTab: {lockRankScavenge, lockRankSweepWaiters, lockRankAssistQueue, lockRankSweep, lockRankSched, lockRankTimers, lockRankHchan, lockRankFin, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankRoot, lockRankTrace},
	lockRankNetpollInit:   {lockRankTimers},

	lockRankRwmutexW: {},
	lockRankRwmutexR: {lockRankRwmutexW},

	lockRankMcentral:     {lockRankScavenge, lockRankForcegc, lockRankAssistQueue, lockRankCpuprof, lockRankSweep, lockRankSched, lockRankAllg, lockRankAllp, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan},
	lockRankSpine:        {lockRankScavenge, lockRankCpuprof, lockRankSched, lockRankAllg, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan},
	lockRankStackpool:    {lockRankScavenge, lockRankSweepWaiters, lockRankAssistQueue, lockRankCpuprof, lockRankSweep, lockRankSched, lockRankPollDesc, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankHchan, lockRankFin, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankProf, lockRankGcBitsArenas, lockRankRoot, lockRankTrace, lockRankTraceStackTab, lockRankNetpollInit, lockRankRwmutexR, lockRankMcentral, lockRankSpine},
	lockRankStackLarge:   {lockRankAssistQueue, lockRankSched, lockRankItab, lockRankHchan, lockRankProf, lockRankGcBitsArenas, lockRankRoot, lockRankMcentral},
	lockRankDefer:        {},
	lockRankSudog:        {lockRankNotifyList, lockRankHchan},
	lockRankWbufSpans:    {lockRankScavenge, lockRankSweepWaiters, lockRankAssistQueue, lockRankSweep, lockRankSched, lockRankAllg, lockRankPollDesc, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankHchan, lockRankNotifyList, lockRankTraceStrings, lockRankMspanSpecial, lockRankProf, lockRankRoot, lockRankDefer, lockRankSudog},
	lockRankMheap:        {lockRankScavenge, lockRankSweepWaiters, lockRankAssistQueue, lockRankCpuprof, lockRankSweep, lockRankSched, lockRankAllg, lockRankAllp, lockRankPollDesc, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan, lockRankMspanSpecial, lockRankProf, lockRankGcBitsArenas, lockRankRoot, lockRankMcentral, lockRankStackpool, lockRankStackLarge, lockRankDefer, lockRankSudog, lockRankWbufSpans},
	lockRankMheapSpecial: {lockRankScavenge, lockRankCpuprof, lockRankSched, lockRankAllg, lockRankAllp, lockRankTimers, lockRankItab, lockRankReflectOffs, lockRankNotifyList, lockRankTraceBuf, lockRankTraceStrings, lockRankHchan},
	lockRankGlobalAlloc:  {lockRankProf, lockRankSpine, lockRankMheap, lockRankMheapSpecial},

	lockRankGFree: {lockRankSched},

	lockRankNewmHandoff:   {},
	lockRankDebugPtrmask:  {},
	lockRankFaketimeState: {},
	lockRankTicks:         {},
	lockRankRaceFini:      {},
	lockRankPollCache:     {},
	lockRankDebug:         {},
}
