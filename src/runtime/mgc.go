// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector (GC).
//
// The GC runs concurrently with mutator threads, is type accurate (aka precise), allows multiple
// GC thread to run in parallel. It is a concurrent mark and sweep that uses a write barrier. It is
// non-generational and non-compacting. Allocation is done using size segregated per P allocation
// areas to minimize fragmentation while eliminating locks in the common case.
//
// The algorithm decomposes into several steps.
// This is a high level description of the algorithm being used. For an overview of GC a good
// place to start is Richard Jones' gchandbook.org.
//
// The algorithm's intellectual heritage includes Dijkstra's on-the-fly algorithm, see
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation. Commun. ACM 21, 11 (November 1978),
// 966-975.
// For journal quality proofs that these steps are complete, correct, and terminate see
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
//
// 1. GC performs sweep termination.
//
//    a. Stop the world. This causes all Ps to reach a GC safe-point.
//
//    b. Sweep any unswept spans. There will only be unswept spans if
//    this GC cycle was forced before the expected time.
//
// 2. GC performs the mark phase.
//
//    a. Prepare for the mark phase by setting gcphase to _GCmark
//    (from _GCoff), enabling the write barrier, enabling mutator
//    assists, and enqueueing root mark jobs. No objects may be
//    scanned until all Ps have enabled the write barrier, which is
//    accomplished using STW.
//
//    b. Start the world. From this point, GC work is done by mark
//    workers started by the scheduler and by assists performed as
//    part of allocation. The write barrier shades both the
//    overwritten pointer and the new pointer value for any pointer
//    writes (see mbarrier.go for details). Newly allocated objects
//    are immediately marked black.
//
//    c. GC performs root marking jobs. This includes scanning all
//    stacks, shading all globals, and shading any heap pointers in
//    off-heap runtime data structures. Scanning a stack stops a
//    goroutine, shades any pointers found on its stack, and then
//    resumes the goroutine.
//
//    d. GC drains the work queue of grey objects, scanning each grey
//    object to black and shading all pointers found in the object
//    (which in turn may add those pointers to the work queue).
//
//    e. Because GC work is spread across local caches, GC uses a
//    distributed termination algorithm to detect when there are no
//    more root marking jobs or grey objects (see gcMarkDone). At this
//    point, GC transitions to mark termination.
//
// 3. GC performs mark termination.
//
//    a. Stop the world.
//
//    b. Set gcphase to _GCmarktermination, and disable workers and
//    assists.
//
//    c. Perform housekeeping like flushing mcaches.
//
// 4. GC performs the sweep phase.
//
//    a. Prepare for the sweep phase by setting gcphase to _GCoff,
//    setting up sweep state and disabling the write barrier.
//
//    b. Start the world. From this point on, newly allocated objects
//    are white, and allocating sweeps spans before use if necessary.
//
//    c. GC does concurrent sweeping in the background and in response
//    to allocation. See description below.
//
// 5. When sufficient allocation has taken place, replay the sequence
// starting with 1 above. See discussion of GC rate below.

// Concurrent sweep.
//
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs another span)
// and concurrently in a background goroutine (this helps programs that are not CPU bound).
// At the end of STW mark termination all spans are marked as "needs sweeping".
//
// The background sweeper goroutine simply sweeps spans one-by-one.
//
// To avoid requesting more OS memory while there are unswept spans, when a
// goroutine needs another span, it first attempts to reclaim that much memory
// by sweeping. When a goroutine needs to allocate a new small-object span, it
// sweeps small-object spans for the same object size until it frees at least
// one object. When a goroutine needs to allocate large-object span from heap,
// it sweeps spans until it frees at least that many pages into heap. There is
// one case where this may not suffice: if a goroutine sweeps and frees two
// nonadjacent one-page spans to the heap, it will allocate a new two-page
// span, but there can still be other one-page unswept spans which could be
// combined into a two-page span.
//
// It's critical to ensure that no operations proceed on unswept spans (that would corrupt
// mark bits in GC bitmap). During GC all mcaches are flushed into the central cache,
// so they are empty. When a goroutine grabs a new span into mcache, it sweeps it.
// When a goroutine explicitly frees an object or sets a finalizer, it ensures that
// the span is swept (either by sweeping it, or by waiting for the concurrent sweep to finish).
// The finalizer goroutine is kicked off only when all spans are swept.
// When the next GC starts, it sweeps all not-yet-swept spans (if any).

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment variable
// (100 by default). If GOGC=100 and we're using 4M, we'll GC again when we get to 8M
// (this mark is tracked in gcController.heapGoal variable). This keeps the GC cost in
// linear proportion to the allocation cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).

// Oblets
//
// In order to prevent long pauses while scanning large objects and to
// improve parallelism, the garbage collector breaks up scan jobs for
// objects larger than maxObletBytes into "oblets" of at most
// maxObletBytes. When scanning encounters the beginning of a large
// object, it scans only the first oblet and enqueues the remaining
// oblets as new scan jobs.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

const (
	_DebugGC         = 0
	_ConcurrentSweep = true
	_FinBlockSize    = 4 * 1024

	// debugScanConservative enables debug logging for stack
	// frames that are scanned conservatively.
	debugScanConservative = false

	// sweepMinHeapDistance is a lower bound on the heap distance
	// (in bytes) reserved for concurrent sweeping between GC
	// cycles.
	sweepMinHeapDistance = 1024 * 1024
)
// gcinit gc初始化
func gcinit() {
	if unsafe.Sizeof(workbuf{}) != _WorkbufSize {
		throw("size of Workbuf is suboptimal")
	}
	// No sweep on the first cycle.
	mheap_.sweepDrained = 1

	// 初始化gc控制器
	// Initialize GC pacer state.
	// Use the environment variable GOGC for the initial gcPercent value.
	gcController.init(readGOGC())

	work.startSema = 1
	work.markDoneSema = 1
	lockInit(&work.sweepWaiters.lock, lockRankSweepWaiters)
	lockInit(&work.assistQueue.lock, lockRankAssistQueue)
	lockInit(&work.wbufSpans.lock, lockRankWbufSpans)
}

// Temporary in order to enable register ABI work.
// TODO(register args): convert back to local chan in gcenabled, passed to "go" stmts.
var gcenable_setup chan int

// gc 启动一个goroutine进行gc清扫
// gcenable is called after the bulk of the runtime initialization,
// just before we're about to start letting user code run.
// It kicks off the background sweeper goroutine, the background
// scavenger goroutine, and enables GC.
func gcenable() {
	// Kick off sweeping and scavenging.
	gcenable_setup = make(chan int, 2)
	go bgsweep()
	go bgscavenge()
	<-gcenable_setup
	<-gcenable_setup
	gcenable_setup = nil
	memstats.enablegc = true // now that runtime is initialized, GC is okay
}

// Garbage collector phase.
// Indicates to write barrier and synchronization task to perform.
var gcphase uint32

// 写屏障
// The compiler knows about this variable.
// If you change it, you must change builtin/runtime.go, too.
// If you change the first four bytes, you must also change the write
// barrier insertion code.
var writeBarrier struct {
	enabled bool    // compiler emits a check of this before calling write barrier
	pad     [3]byte // compiler uses 32-bit load for "enabled" field
	needed  bool    // whether we need a write barrier for current GC phase
	cgo     bool    // whether we need a write barrier for a cgo check
	alignme uint64  // guarantee alignment so that compiler can use a 32 or 64-bit load
}

// gcBlackenEnabled is 1 if mutator assists and background mark
// workers are allowed to blacken objects. This must only be set when
// gcphase == _GCmark.
var gcBlackenEnabled uint32

const (
	_GCoff             = iota // GC not running; sweeping in background, write barrier disabled
	_GCmark                   // GC marking roots and workbufs: allocate black, write barrier ENABLED
	_GCmarktermination        // GC mark termination: allocate black, P's help GC, write barrier ENABLED
)

// 设置阶段状态，按需启用或禁止写屏障
//go:nosplit
func setGCPhase(x uint32) {
	atomic.Store(&gcphase, x)
	writeBarrier.needed = gcphase == _GCmark || gcphase == _GCmarktermination
	writeBarrier.enabled = writeBarrier.needed || writeBarrier.cgo
}

// gcMarkWorkerMode represents the mode that a concurrent mark worker
// should operate in.
//
// Concurrent marking happens through four different mechanisms. One
// is mutator assists, which happen in response to allocations and are
// not scheduled. The other three are variations in the per-P mark
// workers and are distinguished by gcMarkWorkerMode.
// 标记工人的工作模式
type gcMarkWorkerMode int

const (
	// gcMarkWorkerNotWorker indicates that the next scheduled G is not
	// starting work and the mode should be ignored.
	gcMarkWorkerNotWorker gcMarkWorkerMode = iota

	// 专职清理工，不会被抢占
	// gcMarkWorkerDedicatedMode indicates that the P of a mark
	// worker is dedicated to running that mark worker. The mark
	// worker should run without preemption.
	gcMarkWorkerDedicatedMode

	// 小时工，参与少量工作，可被抢占
	// gcMarkWorkerFractionalMode indicates that a P is currently
	// running the "fractional" mark worker. The fractional worker
	// is necessary when GOMAXPROCS*gcBackgroundUtilization is not
	// an integer and using only dedicated workers would result in
	// utilization too far from the target of gcBackgroundUtilization.
	// The fractional worker should run until it is preempted and
	// will be scheduled to pick up the fractional part of
	// GOMAXPROCS*gcBackgroundUtilization.
	gcMarkWorkerFractionalMode

	// 临时工，找不到其他任务，临时参与工作，可被抢占
	// gcMarkWorkerIdleMode indicates that a P is running the mark
	// worker because it has nothing else to do. The idle worker
	// should run until it is preempted and account its time
	// against gcController.idleMarkTime.
	gcMarkWorkerIdleMode
)

// gcMarkWorkerModeStrings are the strings labels of gcMarkWorkerModes
// to use in execution traces.
var gcMarkWorkerModeStrings = [...]string{
	"Not worker",
	"GC (dedicated)",
	"GC (fractional)",
	"GC (idle)",
}

// pollFractionalWorkerExit reports whether a fractional mark worker
// should self-preempt. It assumes it is called from the fractional
// worker.
func pollFractionalWorkerExit() bool {
	// This should be kept in sync with the fractional worker
	// scheduler logic in findRunnableGCWorker.
	now := nanotime()
	delta := now - gcController.markStartTime
	if delta <= 0 {
		return true
	}
	p := getg().m.p.ptr()
	selfTime := p.gcFractionalMarkTime + (now - p.gcMarkWorkerStartTime)
	// Add some slack to the utilization goal so that the
	// fractional worker isn't behind again the instant it exits.
	return float64(selfTime)/float64(delta) > 1.2*gcController.fractionalUtilizationGoal
}
// 全局gc工作 struct
var work struct {
	// ls->[*workbuf,*]->runtime.workbufhdr
	full  lfstack          // lock-free list of full blocks workbuf
	empty lfstack          // lock-free list of empty blocks workbuf
	pad0  cpu.CacheLinePad // prevents false-sharing between full/empty and nproc/nwait

	wbufSpans struct {
		lock mutex
		// free is a list of spans dedicated to workbufs, but
		// that don't currently contain any workbufs.
		free mSpanList
		// busy is a list of all spans containing workbufs on
		// one of the workbuf lists.
		busy mSpanList
	}

	// Restore 64-bit alignment on 32-bit.
	_ uint32

	// bytesMarked is the number of bytes marked this cycle. This
	// includes bytes blackened in scanned objects, noscan objects
	// that go straight to black, and permagrey objects scanned by
	// markroot during the concurrent scan phase. This is updated
	// atomically during the cycle. Updates may be batched
	// arbitrarily, since the value is only read at the end of the
	// cycle.
	//
	// Because of benign races during marking, this number may not
	// be the exact number of marked bytes, but it should be very
	// close.
	//
	// Put this field here because it needs 64-bit atomic access
	// (and thus 8-byte alignment even on 32-bit architectures).
	bytesMarked uint64

	markrootNext uint32 // next markroot job
	markrootJobs uint32 // number of markroot jobs

	nproc  uint32
	tstart int64
	nwait  uint32

	// Number of roots of various root types. Set by gcMarkRootPrepare.
	nDataRoots, nBSSRoots, nSpanRoots, nStackRoots int

	// Base indexes of each root type. Set by gcMarkRootPrepare.
	baseData, baseBSS, baseSpans, baseStacks, baseEnd uint32

	// Each type of GC state transition is protected by a lock.
	// Since multiple threads can simultaneously detect the state
	// transition condition, any thread that detects a transition
	// condition must acquire the appropriate transition lock,
	// re-check the transition condition and return if it no
	// longer holds or perform the transition if it does.
	// Likewise, any transition must invalidate the transition
	// condition before releasing the lock. This ensures that each
	// transition is performed by exactly one thread and threads
	// that need the transition to happen block until it has
	// happened.
	//
	// startSema protects the transition from "off" to mark or
	// mark termination.
	startSema uint32
	// markDoneSema protects transitions from mark to mark termination.
	markDoneSema uint32

	bgMarkReady note   // signal background mark worker has started
	bgMarkDone  uint32 // cas to 1 when at a background mark completion point
	// Background mark completion signaling

	// mode is the concurrency mode of the current GC cycle.
	mode gcMode

	// userForced indicates the current GC cycle was forced by an
	// explicit user call.
	userForced bool

	// totaltime is the CPU nanoseconds spent in GC since the
	// program started if debug.gctrace > 0.
	totaltime int64

	// initialHeapLive is the value of gcController.heapLive at the
	// beginning of this GC cycle.
	initialHeapLive uint64

	// assistQueue is a queue of assists that are blocked because
	// there was neither enough credit to steal or enough work to
	// do.
	assistQueue struct {
		lock mutex
		q    gQueue
	}

	// sweepWaiters is a list of blocked goroutines to wake when
	// we transition from mark termination to sweep.
	sweepWaiters struct {
		lock mutex
		list gList
	}

	// cycles is the number of completed GC cycles, where a GC
	// cycle is sweep termination, mark, mark termination, and
	// sweep. This differs from memstats.numgc, which is
	// incremented at mark termination.
	cycles uint32

	// Timing/utilization stats for this cycle.
	stwprocs, maxprocs                 int32
	tSweepTerm, tMark, tMarkTerm, tEnd int64 // nanotime() of phase start

	pauseNS    int64 // total STW time this cycle
	pauseStart int64 // nanotime() of last STW

	// debug.gctrace heap sizes for this cycle.
	heap0, heap1, heap2, heapGoal uint64
}

// 手动触发GC
// GC runs a garbage collection and blocks the caller until the
// garbage collection is complete. It may also block the entire
// program.
func GC() {
	// 手动调用runtime.GC时，可能后台正在执行第N次垃圾回收。不管N处于回收周期
	// 的那个阶段，都应该先等它解说。直至N彻底完成后，才开始我们的N+1回收周期
	// 知道清理(sweep)结束。
	// 注意：该函数并未调用test检查触发条件且会阻塞用户代码。
	// We consider a cycle to be: sweep termination, mark, mark
	// termination, and sweep. This function shouldn't return
	// until a full cycle has been completed, from beginning to
	// end. Hence, we always want to finish up the current cycle
	// and start a new one. That means:
	//
	// 1. In sweep termination, mark, or mark termination of cycle
	// N, wait until mark termination N completes and transitions
	// to sweep N.
	//
	// 2. In sweep N, help with sweep N.
	//
	// At this point we can begin a full cycle N+1.
	//
	// 3. Trigger cycle N+1 by starting sweep termination N+1.
	//
	// 4. Wait for mark termination N+1 to complete.
	//
	// 5. Help with sweep N+1 until it's done.
	//
	// This all has to be written to deal with the fact that the
	// GC may move ahead on its own. For example, when we block
	// until mark termination N, we may wake up in cycle N+2.

	// Wait until the current sweep termination, mark, and mark
	// termination complete.
	// 等待N标记结束
	n := atomic.Load(&work.cycles)
	gcWaitOnMark(n)

	// We're now in sweep N or later. Trigger GC cycle N+1, which
	// will first finish sweep N if necessary and then enter sweep
	// termination N+1.
	// 启动N+1周期
	// 并没有进行t.test()函数检查触发条件，因为runtime.GC即为手动强制触发，就是无论如何都要进行GC
	gcStart(gcTrigger{kind: gcTriggerCycle, n: n + 1})

	// 等待N+1标记结束
	// Wait for mark termination N+1 to complete.
	gcWaitOnMark(n + 1)

	// 执行清理任务
	// Finish sweep N+1 before returning. We do this both to
	// complete the cycle and because runtime.GC() is often used
	// as part of tests and benchmarks to get the system into a
	// relatively stable and isolated state.
	for atomic.Load(&work.cycles) == n+1 && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++ // 非后台清理
		Gosched()
	}

	// Callers may assume that the heap profile reflects the
	// just-completed cycle when this returns (historically this
	// happened because this was a STW GC), but right now the
	// profile still reflects mark termination N, not N+1.
	//
	// As soon as all of the sweep frees from cycle N+1 are done,
	// we can go ahead and publish the heap profile.
	//
	// First, wait for sweeping to finish. (We know there are no
	// more spans on the sweep queue, but we may be concurrently
	// sweeping spans, so we have to wait.)
	// 等待清理结束
	for atomic.Load(&work.cycles) == n+1 && !isSweepDone() {
		Gosched()
	}

	// Now we're really done with sweeping, so we can publish the
	// stable heap profile. Only do this if we haven't already hit
	// another mark termination.
	mp := acquirem()
	cycle := atomic.Load(&work.cycles)
	if cycle == n+1 || (gcphase == _GCmark && cycle == n+2) {
		mProf_PostSweep()
	}
	releasem(mp)
}

// gcWaitOnMark blocks until GC finishes the Nth mark phase. If GC has
// already completed this mark phase, it returns immediately.
func gcWaitOnMark(n uint32) {
	for {
		// Disable phase transitions.
		lock(&work.sweepWaiters.lock)
		// 当前周期序号
		nMarks := atomic.Load(&work.cycles)
		// 若已标记完成
		if gcphase != _GCmark {
			// We've already completed this cycle's mark.
			nMarks++
		}
		// 当前周期超过n，结束
		if nMarks > n {
			// We're done.
			unlock(&work.sweepWaiters.lock)
			return
		}

		// Wait until sweep termination, mark, and mark
		// termination of cycle N complete.
		work.sweepWaiters.list.push(getg())
		goparkunlock(&work.sweepWaiters.lock, waitReasonWaitForGCCycle, traceEvGoBlock, 1)
	}
}

// GC工作模式
// gcMode indicates how concurrent a GC cycle should be.
type gcMode int

const (
	// 后台并发标记扫描
	gcBackgroundMode gcMode = iota // concurrent GC and sweep
	// 非阻塞STW GC开始
	gcForceMode                    // stop-the-world GC now, concurrent sweep
	// 阻塞G STW GC开始
	gcForceBlockMode               // stop-the-world GC now and STW sweep (forced by user)
)

// A gcTrigger is a predicate for starting a GC cycle. Specifically,
// it is an exit condition for the _GCoff phase.
type gcTrigger struct {
	kind gcTriggerKind
	now  int64  // gcTriggerTime: current time
	n    uint32 // gcTriggerCycle: cycle number to start
}

type gcTriggerKind int
// 垃圾回收开始的方式
const (
	// gcTriggerHeap indicates that a cycle should be started when
	// the heap size reaches the trigger heap size computed by the
	// controller.
	// runtime.mallocgc触发设置
	gcTriggerHeap gcTriggerKind = iota

	// gcTriggerTime indicates that a cycle should be started when
	// it's been more than forcegcperiod nanoseconds since the
	// previous GC cycle.
	// runtime.sysmon监控线程触发设置
	gcTriggerTime

	// gcTriggerCycle indicates that a cycle should be started if
	// we have not yet started cycle number gcTrigger.n (relative
	// to work.cycles).
	// runtime.GC触发时设置
	gcTriggerCycle
)

// test reports whether the trigger condition is satisfied, meaning
// that the exit condition for the _GCoff phase has been met. The exit
// condition should be tested when allocating.
// 测试是否达到了GC的启动阈值
func (t gcTrigger) test() bool {
	if !memstats.enablegc || panicking != 0 || gcphase != _GCoff {
		return false
	}
	switch t.kind {
	case gcTriggerHeap:
		// Non-atomic access to gcController.heapLive for performance. If
		// we are going to trigger on this, this thread just
		// atomically wrote gcController.heapLive anyway and we'll see our
		// own write.
		return gcController.heapLive >= gcController.trigger
	case gcTriggerTime:
		// GOGC < 0 OFF
		if gcController.gcPercent < 0 {
			return false
		}
		// 超过两分钟未触发垃圾回收
		lastgc := int64(atomic.Load64(&memstats.last_gc_nanotime))
		return lastgc != 0 && t.now-lastgc > forcegcperiod
	case gcTriggerCycle:
		// t.n > work.cycles, but accounting for wraparound.
		return int32(t.n-work.cycles) > 0
	}
	return true
}

// gcStart starts the GC. It transitions from _GCoff to _GCmark (if
// debug.gcstoptheworld == 0) or performs all of GC (if
// debug.gcstoptheworld != 0).
//
// This may return without performing this transition in some cases,
// such as when called on a system stack or with locks held.
// GC启动以下三种情况触发
// 分配内存时：runtime.mallocgc
// 手动强制触发：runtime.GC
// 定时强制触发：runtime.forcegchelper
func gcStart(trigger gcTrigger) {
	// Since this is called from malloc and malloc is called in
	// the guts of a number of libraries that might be holding
	// locks, don't attempt to start GC in non-preemptible or
	// potentially unstable situations.
	mp := acquirem()
	// 若当前g为m的g0协程，也就是系统线程的堆栈，
	// 或者当前m被锁定，或者当前m正在允许goroutine，不参与垃圾回收
	if gp := getg(); gp == mp.g0 || mp.locks > 1 || mp.preemptoff != "" {
		releasem(mp)
		return
	}
	releasem(mp)
	mp = nil

	// Pick up the remaining unswept/not being swept spans concurrently
	//
	// This shouldn't happen if we're being invoked in background
	// mode since proportional sweep should have just finished
	// sweeping everything, but rounding errors, etc, may leave a
	// few spans unswept. In forced mode, this is necessary since
	// GC can be forced at any point in the sweeping cycle.
	//
	// We check the transition condition continuously here in case
	// this G gets delayed in to the next GC cycle.
	// 检查启动条件，并完成前次清理工作
	for trigger.test() && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++
	}

	// 有可能多个线程检查到触发条件
	// 加锁，并再次执行条件检查，使多余的触发提前退出
	// Perform GC initialization and the sweep termination
	// transition.
	semacquire(&work.startSema)
	// Re-check transition condition under transition lock.
	if !trigger.test() {
		semrelease(&work.startSema)
		return
	}

	// 是否为runtime.GC 手动触发
	// For stats, check if this GC was forced by the user.
	work.userForced = trigger.kind == gcTriggerCycle

	// In gcstoptheworld debug mode, upgrade the mode accordingly.
	// We do this after re-checking the transition condition so
	// that multiple goroutines that detect the heap trigger don't
	// start multiple STW GCs.
	// 根据环境变量 GODEBUG=gcstoptheword 确定回收模式
	mode := gcBackgroundMode
	// 判断GODEBUG环境变量
	// 1:禁用并发标记
	// 2：禁用并发标记和并发清理
	if debug.gcstoptheworld == 1 {
		mode = gcForceMode
	} else if debug.gcstoptheworld == 2 {
		mode = gcForceBlockMode
	}
	// 准备STW
	// Ok, we're doing it! Stop everybody else
	semacquire(&gcsema)
	semacquire(&worldsema)

	if trace.enabled {
		traceGCStart()
	}

	// Check that all Ps have finished deferred mcache flushes.
	for _, p := range allp {
		if fg := atomic.Load(&p.mcache.flushGen); fg != mheap_.sweepgen {
			println("runtime: p", p.id, "flushGen", fg, "!= sweepgen", mheap_.sweepgen)
			throw("p mcache not flushed")
		}
	}
	// 创建标记工人 (mark goroutines)
	gcBgMarkStartWorkers()
	// 重置标记状态
	systemstack(gcResetMarkState)

	work.stwprocs, work.maxprocs = gomaxprocs, gomaxprocs
	if work.stwprocs > ncpu {
		// This is used to compute CPU time of the STW phases,
		// so it can't be more than ncpu, even if GOMAXPROCS is.
		work.stwprocs = ncpu
	}
	work.heap0 = atomic.Load64(&gcController.heapLive)
	work.pauseNS = 0 // 本周起 STW 暂停时间总计
	work.mode = mode // 回收模式

	now := nanotime()
	work.tSweepTerm = now // 前次清理结束时间
	work.pauseStart = now //  本次 STW 开启时间
	if trace.enabled {
		traceGCSTWStart(1)
	}
	// 进入STW
	systemstack(stopTheWorldWithSema)
	// 确保清理结束
	// Finish sweep before we start concurrent scan.
	systemstack(func() {
		finishsweep_m()
	})

	// 处理sync.Pool、sudog 等缓存
	// clearpools before we start the GC. If we wait they memory will not be
	// reclaimed until the next GC cycle.
	clearpools()

	// 周期计数
	work.cycles++
	// 控制器本次周期开始
	gcController.startCycle()
	work.heapGoal = gcController.heapGoal

	// In STW mode, disable scheduling of user Gs. This may also
	// disable scheduling of this goroutine, so it may block as
	// soon as we start the world again.
	// 如果不是后台清扫模式，则关闭用户g的调度，但是正在执行的g不停止
	if mode != gcBackgroundMode {
		schedEnableUser(false)
	}

	// 进入并发标记阶段(启用写屏障)
	// Enter concurrent mark phase and enable
	// write barriers.
	//
	// Because the world is stopped, all Ps will
	// observe that write barriers are enabled by
	// the time we start the world and begin
	// scanning.
	//
	// Write barriers must be enabled before assists are
	// enabled because they must be enabled before
	// any non-leaf heap objects are marked. Since
	// allocations are blocked until assists can
	// happen, we want enable assists as early as
	// possible.
	setGCPhase(_GCmark)
	// 准备相关数据
	gcBgMarkPrepare() // Must happen before assist enable.
	// 准备好需要扫描的根对象，包括运行和等待中的 G 栈
	gcMarkRootPrepare()

	// 微小对象分配看被 cache 持有，所以直接加入灰色队列
	// Mark all active tinyalloc blocks. Since we're
	// allocating from these, they need to be black like
	// other allocations. The alternative is to blacken
	// the tiny block on every allocation from it, which
	// would slow down the tiny allocator.
	gcMarkTinyAllocs()

	// 允许将对象标记为黑色(启用辅助回收)
	// At this point all Ps have enabled the write
	// barrier, thus maintaining the no white to
	// black invariant. Enable mutator assists to
	// put back-pressure on fast allocating
	// mutators.
	atomic.Store(&gcBlackenEnabled, 1)

	// Assists and workers can start the moment we start
	// the world.
	gcController.markStartTime = now

	// In STW mode, we could block the instant systemstack
	// returns, so make sure we're not preemptible.
	mp = acquirem()
	// 解除 STW，以便 runtime.schedule 调度标记工人进行回收作业
	// Concurrent mark.
	systemstack(func() {
		now = startTheWorldWithSema(trace.enabled)
		// 累加 STW 暂停时间
		work.pauseNS += now - work.pauseStart
		// 标记开始时间
		work.tMark = now
		memstats.gcPauseDist.record(now - work.pauseStart)
	})

	// Release the world sema before Gosched() in STW mode
	// because we will need to reacquire it later but before
	// this goroutine becomes runnable again, and we could
	// self-deadlock otherwise.
	semrelease(&worldsema)
	releasem(mp)

	// 非并发模式下，当前G被暂停
	// Make sure we block instead of returning to user code
	// in STW mode.
	if mode != gcBackgroundMode {
		Gosched()
	}
	// 释放锁使得其他线程可以进入
	semrelease(&work.startSema)
}

// gcMarkDoneFlushed counts the number of P's with flushed work.
//
// Ideally this would be a captured local in gcMarkDone, but forEachP
// escapes its callback closure, so it can't capture anything.
//
// This is protected by markDoneSema.
var gcMarkDoneFlushed uint32

// gcMarkDone transitions the GC from mark to mark termination if all
// reachable objects have been marked (that is, there are no grey
// objects and can be no more in the future). Otherwise, it flushes
// all local work to the global queues where it can be discovered by
// other workers.
//
// This should be called when all local mark work has been drained and
// there are no remaining workers. Specifically, when
//
//   work.nwait == work.nproc && !gcMarkWorkAvailable(p)
//
// The calling context must be preemptible.
//
// Flushing local work is important because idle Ps may have local
// work queued. This is the only way to make that work visible and
// drive GC to completion.
//
// It is explicitly okay to have write barriers in this function. If
// it does transition to mark termination, then all reachable objects
// have been marked, so the write barrier cannot shade any more
// objects.
func gcMarkDone() {
	// 只能有一个线程进入
	// Ensure only one thread is running the ragged barrier at a
	// time.
	semacquire(&work.markDoneSema)

top:
	// Re-check transition condition under transition lock.
	//
	// It's critical that this checks the global work queues are
	// empty before performing the ragged barrier. Otherwise,
	// there could be global work that a P could take after the P
	// has passed the ragged barrier.
	// 再次检查。如任务未完成，则不能终止
	if !(gcphase == _GCmark && work.nwait == work.nproc && !gcMarkWorkAvailable(nil)) {
		semrelease(&work.markDoneSema)
		return
	}

	// forEachP needs worldsema to execute, and we'll need it to
	// stop the world later, so acquire worldsema now.
	semacquire(&worldsema)

	// Flush all local buffers and collect flushedWork flags.
	gcMarkDoneFlushed = 0
	systemstack(func() {
		// 变更当前g状态
		gp := getg().m.curg
		// Mark the user stack as preemptible so that it may be scanned.
		// Otherwise, our attempt to force all P's to a safepoint could
		// result in a deadlock as we attempt to preempt a worker that's
		// trying to preempt us (e.g. for a stack scan).
		casgstatus(gp, _Grunning, _Gwaiting)
		// 确保所有 P 处理好本地任务
		forEachP(func(_p_ *p) {
			// Flush the write barrier buffer, since this may add
			// work to the gcWork.
			wbBufFlush1(_p_)

			// Flush the gcWork, since this may create global work
			// and set the flushedWork flag.
			//
			// TODO(austin): Break up these workbufs to
			// better distribute work.
			_p_.gcw.dispose()
			// Collect the flushedWork flag.
			if _p_.gcw.flushedWork {
				atomic.Xadd(&gcMarkDoneFlushed, 1)
				_p_.gcw.flushedWork = false
			}
		})
		casgstatus(gp, _Gwaiting, _Grunning)
	})

	if gcMarkDoneFlushed != 0 {
		// More grey objects were discovered since the
		// previous termination check, so there may be more
		// work to do. Keep going. It's possible the
		// transition condition became true again during the
		// ragged barrier, so re-check it.
		semrelease(&worldsema)
		goto top
	}

	// 记录结束时间，并再次 STW !!!
	// There was no global work, no local work, and no Ps
	// communicated work since we took markDoneSema. Therefore
	// there are no grey objects and no more objects can be
	// shaded. Transition to mark termination.
	now := nanotime()
	work.tMarkTerm = now
	work.pauseStart = now
	getg().m.preemptoff = "gcing"
	if trace.enabled {
		traceGCSTWStart(0)
	}
	// 再次 STW
	systemstack(stopTheWorldWithSema)
	// The gcphase is _GCmark, it will transition to _GCmarktermination
	// below. The important thing is that the wb remains active until
	// all marking is complete. This includes writes made by the GC.

	// There is sometimes work left over when we enter mark termination due
	// to write barriers performed after the completion barrier above.
	// Detect this and resume concurrent mark. This is obviously
	// unfortunate.
	//
	// See issue #27993 for details.
	//
	// Switch to the system stack to call wbBufFlush1, though in this case
	// it doesn't matter because we're non-preemptible anyway.
	restart := false
	systemstack(func() {
		for _, p := range allp {
			wbBufFlush1(p)
			if !p.gcw.empty() {
				restart = true
				break
			}
		}
	})
	if restart {
		getg().m.preemptoff = ""
		systemstack(func() {
			now := startTheWorldWithSema(true)
			work.pauseNS += now - work.pauseStart
			memstats.gcPauseDist.record(now - work.pauseStart)
		})
		semrelease(&worldsema)
		goto top
	}

	// 禁用工人和辅助
	// Disable assists and background workers. We must do
	// this before waking blocked assists.
	atomic.Store(&gcBlackenEnabled, 0)

	// 唤醒被休眠的辅助
	// Wake all blocked assists. These will run when we
	// start the world again.
	gcWakeAllAssists()

	// Likewise, release the transition lock. Blocked
	// workers and assists will run when we start the
	// world again.
	semrelease(&work.markDoneSema)

	// In STW mode, re-enable user goroutines. These will be
	// queued to run after we start the world.
	schedEnableUser(true)

	// 本周期结束
	// endCycle depends on all gcWork cache stats being flushed.
	// The termination algorithm above ensured that up to
	// allocations since the ragged barrier.
	nextTriggerRatio := gcController.endCycle(work.userForced)

	// 标记周期结束后，启动清理任务
	// Perform mark termination. This will restart the world.
	gcMarkTermination(nextTriggerRatio)
}

// 标记阶段结束，开启清理
// World must be stopped and mark assists and background workers must be
// disabled.
func gcMarkTermination(nextTriggerRatio float64) {
	// 并发状态下
	// Start marktermination (write barrier remains enabled for now).
	setGCPhase(_GCmarktermination)

	work.heap1 = gcController.heapLive
	startTime := nanotime()

	mp := acquirem()
	mp.preemptoff = "gcing"
	_g_ := getg()
	_g_.m.traceback = 2
	gp := _g_.m.curg
	casgstatus(gp, _Grunning, _Gwaiting)
	gp.waitreason = waitReasonGarbageCollection

	// Run gc on the g0 stack. We do this so that the g stack
	// we're currently running on will no longer change. Cuts
	// the root set down a bit (g0 stacks are not scanned, and
	// we don't need to scan gc's internal state).  We also
	// need to switch to g0 so we can shrink the stack.
	systemstack(func() {
		gcMark(startTime)
		// Must return immediately.
		// The outer function's stack may have moved
		// during gcMark (it shrinks stacks, including the
		// outer function's stack), so we must not refer
		// to any of its variables. Return back to the
		// non-system stack to pick up the new addresses
		// before continuing.
	})

	// 标记结束(禁用写屏障)，开启清理
	systemstack(func() {
		work.heap2 = work.bytesMarked
		if debug.gccheckmark > 0 {
			// Run a full non-parallel, stop-the-world
			// mark using checkmark bits, to check that we
			// didn't forget to mark anything during the
			// concurrent mark process.
			startCheckmarks()
			gcResetMarkState()
			gcw := &getg().m.p.ptr().gcw
			gcDrain(gcw, 0)
			wbBufFlush1(getg().m.p.ptr())
			gcw.dispose()
			endCheckmarks()
		}

		// 标记结束
		// marking is complete so we can turn the write barrier off
		setGCPhase(_GCoff)
		// 开始清理
		gcSweep(work.mode)
	})

	_g_.m.traceback = 0
	casgstatus(gp, _Gwaiting, _Grunning)

	if trace.enabled {
		traceGCDone()
	}

	// all done
	mp.preemptoff = ""

	if gcphase != _GCoff {
		throw("gc done but gcphase != _GCoff")
	}

	// Record heapGoal and heap_inuse for scavenger.
	gcController.lastHeapGoal = gcController.heapGoal
	memstats.last_heap_inuse = memstats.heap_inuse

	// Update GC trigger and pacing for the next cycle.
	gcController.commit(nextTriggerRatio)

	// Update timing memstats
	now := nanotime()
	sec, nsec, _ := time_now()
	unixNow := sec*1e9 + int64(nsec)
	work.pauseNS += now - work.pauseStart
	work.tEnd = now
	memstats.gcPauseDist.record(now - work.pauseStart)
	atomic.Store64(&memstats.last_gc_unix, uint64(unixNow)) // must be Unix time to make sense to user
	atomic.Store64(&memstats.last_gc_nanotime, uint64(now)) // monotonic time for us
	memstats.pause_ns[memstats.numgc%uint32(len(memstats.pause_ns))] = uint64(work.pauseNS)
	memstats.pause_end[memstats.numgc%uint32(len(memstats.pause_end))] = uint64(unixNow)
	memstats.pause_total_ns += uint64(work.pauseNS)

	// Update work.totaltime.
	sweepTermCpu := int64(work.stwprocs) * (work.tMark - work.tSweepTerm)
	// We report idle marking time below, but omit it from the
	// overall utilization here since it's "free".
	markCpu := gcController.assistTime + gcController.dedicatedMarkTime + gcController.fractionalMarkTime
	markTermCpu := int64(work.stwprocs) * (work.tEnd - work.tMarkTerm)
	cycleCpu := sweepTermCpu + markCpu + markTermCpu
	work.totaltime += cycleCpu

	// Compute overall GC CPU utilization.
	totalCpu := sched.totaltime + (now-sched.procresizetime)*int64(gomaxprocs)
	memstats.gc_cpu_fraction = float64(work.totaltime) / float64(totalCpu)

	// Reset sweep state.
	sweep.nbgsweep = 0
	sweep.npausesweep = 0

	if work.userForced {
		memstats.numforcedgc++
	}

	// 唤醒等待标记结束的 Gs (runtime.GC)
	// Bump GC cycle count and wake goroutines waiting on sweep.
	lock(&work.sweepWaiters.lock)
	memstats.numgc++
	injectglist(&work.sweepWaiters.list)
	unlock(&work.sweepWaiters.lock)

	// Finish the current heap profiling cycle and start a new
	// heap profiling cycle. We do this before starting the world
	// so events don't leak into the wrong cycle.
	mProf_NextCycle()

	// There may be stale spans in mcaches that need to be swept.
	// Those aren't tracked in any sweep lists, so we need to
	// count them against sweep completion until we ensure all
	// those spans have been forced out.
	sl := newSweepLocker()
	sl.blockCompletion()

	// 解除 STW
	systemstack(func() { startTheWorldWithSema(true) })

	// Flush the heap profile so we can start a new cycle next GC.
	// This is relatively expensive, so we don't do it with the
	// world stopped.
	mProf_Flush()

	// 回收workbuf
	// Prepare workbufs for freeing by the sweeper. We do this
	// asynchronously because it can take non-trivial time.
	prepareFreeWorkbufs()

	// 清理stackpool
	// Free stack spans. This must be done between GC cycles.
	systemstack(freeStackSpans)

	// Ensure all mcaches are flushed. Each P will flush its own
	// mcache before allocating, but idle Ps may not. Since this
	// is necessary to sweep all spans, we need to ensure all
	// mcaches are flushed before we start the next GC cycle.
	systemstack(func() {
		// 垃圾回收将将所有 P.cache 持有的mspan交还给central，以便闲置内存可用调度给其他P.cache使用
		forEachP(func(_p_ *p) {
			_p_.mcache.prepareForSweep()
		})
	})

	// 清理 mache.alloc
	// Now that we've swept stale spans in mcaches, they don't
	// count against unswept spans.
	sl.dispose()

	// Print gctrace before dropping worldsema. As soon as we drop
	// worldsema another cycle could start and smash the stats
	// we're trying to print.
	if debug.gctrace > 0 {
		util := int(memstats.gc_cpu_fraction * 100)

		var sbuf [24]byte
		printlock()
		print("gc ", memstats.numgc,
			" @", string(itoaDiv(sbuf[:], uint64(work.tSweepTerm-runtimeInitTime)/1e6, 3)), "s ",
			util, "%: ")
		prev := work.tSweepTerm
		for i, ns := range []int64{work.tMark, work.tMarkTerm, work.tEnd} {
			if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns-prev))))
			prev = ns
		}
		print(" ms clock, ")
		for i, ns := range []int64{sweepTermCpu, gcController.assistTime, gcController.dedicatedMarkTime + gcController.fractionalMarkTime, gcController.idleMarkTime, markTermCpu} {
			if i == 2 || i == 3 {
				// Separate mark time components with /.
				print("/")
			} else if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns))))
		}
		print(" ms cpu, ",
			work.heap0>>20, "->", work.heap1>>20, "->", work.heap2>>20, " MB, ",
			work.heapGoal>>20, " MB goal, ",
			work.maxprocs, " P")
		if work.userForced {
			print(" (forced)")
		}
		print("\n")
		printunlock()
	}

	semrelease(&worldsema)
	semrelease(&gcsema)
	// Careful: another GC cycle may start now.

	releasem(mp)
	mp = nil

	// now that gc is done, kick off finalizer thread if needed
	if !concurrentSweep {
		// give the queued finalizers, if any, a chance to run
		Gosched()
	}
}

// 标记未开始前，准备好与P等量的 worker G.带后续 gcController.startCycle 决定人数和工种
// gcBgMarkStartWorkers prepares background mark worker goroutines. These
// goroutines will not run until the mark phase, but they must be started while
// the work is not stopped and from a regular G stack. The caller must hold
// worldsema.
func gcBgMarkStartWorkers() {
	// Background marking is performed by per-P G's. Ensure that each P has
	// a background GC G.
	//
	// Worker Gs don't exit if gomaxprocs is reduced. If it is raised
	// again, we can reuse the old workers; no need to create new workers.
	// 有多少个 p 就创建多少工作 worker
	for gcBgMarkWorkerCount < gomaxprocs {
		// 创建 worker G
		go gcBgMarkWorker()

		// 休眠，等当这个 worker 准备好在创建下一个
		notetsleepg(&work.bgMarkReady, -1)
		noteclear(&work.bgMarkReady)
		// The worker is now guaranteed to be added to the pool before
		// its P's next findRunnableGCWorker.

		gcBgMarkWorkerCount++
	}
}

// gcBgMarkPrepare sets up state for background marking.
// Mutator assists must not yet be enabled.
func gcBgMarkPrepare() {
	// Background marking will stop when the work queues are empty
	// and there are no more workers (note that, since this is
	// concurrent, this may be a transient state, but mark
	// termination will clean it up). Between background workers
	// and assists, we don't really know how many workers there
	// will be, so we pretend to have an arbitrarily large number
	// of workers, almost all of which are "waiting". While a
	// worker is working it decrements nwait. If nproc == nwait,
	// there are no workers.
	// 当两个变量相同时，表示所有人都在等待工作，即工作完成了
	work.nproc = ^uint32(0) // 需要多少人工作
	work.nwait = ^uint32(0) // 有多少人在等待
}

// gcBgMarkWorker is an entry in the gcBgMarkWorkerPool. It points to a single
// gcBgMarkWorker goroutine.
// 新创建的后台标记工人保存在池内，休眠待用
type gcBgMarkWorkerNode struct {
	// Unused workers are managed in a lock-free stack. This field must be first.
	node lfnode

	// The g of this worker.
	gp guintptr

	// Release this m on park. This is used to communicate with the unlock
	// function, which cannot access the G's stack. It is unused outside of
	// gcBgMarkWorker().
	m muintptr
}

// 创建 worker G
func gcBgMarkWorker() {
	gp := getg()

	// We pass node to a gopark unlock function, so it can't be on
	// the stack (see gopark). Prevent deadlock from recursively
	// starting GC by disabling preemption.
	gp.m.preemptoff = "GC worker init"
	node := new(gcBgMarkWorkerNode)
	gp.m.preemptoff = ""

	node.gp.set(gp)

	node.m.set(acquirem())
	// 唤醒gcBgMarkStartWorkers函数 创建 下一个 worker G
	notewakeup(&work.bgMarkReady)
	// After this point, the background mark worker is generally scheduled
	// cooperatively by gcController.findRunnableGCWorker. While performing
	// work on the P, preemption is disabled because we are working on
	// P-local work buffers. When the preempt flag is set, this puts itself
	// into _Gwaiting to be woken up by gcController.findRunnableGCWorker
	// at the appropriate time.
	//
	// When preemption is enabled (e.g., while in gcMarkDone), this worker
	// may be preempted and schedule as a _Grunnable G from a runq. That is
	// fine; it will eventually gopark again for further scheduling via
	// findRunnableGCWorker.
	//
	// Since we disable preemption before notifying bgMarkReady, we
	// guarantee that this G will be in the worker pool for the next
	// findRunnableGCWorker. This isn't strictly necessary, but it reduces
	// latency between _GCmark starting and the workers starting.
	// 在一个周期内，非专业工种会被抢占退出。而后可能再次，进入标记工作。所以，该循环是必要的。
	// 当一个非专业工种被抢占了，该g会再次进入循环 gopark 等待被gcBgMarkStartWorkers唤醒。
	for {
		// Go to sleep until woken by
		// gcController.findRunnableGCWorker.
		// 休眠，直到被 gcController.findRunnableGcWorker 修改该 g 的状态，并由 schedule函数唤醒
		gopark(func(g *g, nodep unsafe.Pointer) bool {
			// 将当前 worker G 放入池内，供 findRunnableGcWorker 调用
			node := (*gcBgMarkWorkerNode)(nodep)

			if mp := node.m.ptr(); mp != nil {
				// The worker G is no longer running; release
				// the M.
				//
				// N.B. it is _safe_ to release the M as soon
				// as we are no longer performing P-local mark
				// work.
				//
				// However, since we cooperatively stop work
				// when gp.preempt is set, if we releasem in
				// the loop then the following call to gopark
				// would immediately preempt the G. This is
				// also safe, but inefficient: the G must
				// schedule again only to enter gopark and park
				// again. Thus, we defer the release until
				// after parking the G.
				releasem(mp)
			}

			// Release this G to the pool.
			gcBgMarkWorkerPool.push(&node.node)
			// Note that at this point, the G may immediately be
			// rescheduled and may be running.
			return true
		}, unsafe.Pointer(node), waitReasonGCWorkerIdle, traceEvGoBlock, 0)

		// Preemption must not occur here, or another G might see
		// p.gcMarkWorkerMode.

		// 标记工作的细节
		// 唤醒后的准备工作
		// Disable preemption so we can use the gcw. If the
		// scheduler wants to preempt us, we'll stop draining,
		// dispose the gcw, and then preempt.
		node.m.set(acquirem())
		pp := gp.m.p.ptr() // P can't change with preemption disabled.

		if gcBlackenEnabled == 0 {
			println("worker mode", pp.gcMarkWorkerMode)
			throw("gcBgMarkWorker: blackening not enabled")
		}

		if pp.gcMarkWorkerMode == gcMarkWorkerNotWorker {
			throw("gcBgMarkWorker: mode not set")
		}

		startTime := nanotime()
		pp.gcMarkWorkerStartTime = startTime
		// 工作等待工人数 -1，-1相当于把自己减去，
		decnwait := atomic.Xadd(&work.nwait, -1)
		if decnwait == work.nproc {
			println("runtime: work.nwait=", decnwait, "work.nproc=", work.nproc)
			throw("work.nwait was > work.nproc")
		}

		// 工作！
		// runtime.gcDrain gc标记工作
		systemstack(func() {
			// Mark our goroutine preemptible so its stack
			// can be scanned. This lets two mark workers
			// scan each other (otherwise, they would
			// deadlock). We must not modify anything on
			// the G stack. However, stack shrinking is
			// disabled for mark workers, so it is safe to
			// read from the G stack.
			casgstatus(gp, _Grunning, _Gwaiting)
			switch pp.gcMarkWorkerMode {
			default:
				throw("gcBgMarkWorker: unexpected gcMarkWorkerMode")
			case gcMarkWorkerDedicatedMode:
				gcDrain(&pp.gcw, gcDrainUntilPreempt|gcDrainFlushBgCredit)
				if gp.preempt {
					// We were preempted. This is
					// a useful signal to kick
					// everything out of the run
					// queue so it can run
					// somewhere else.
					if drainQ, n := runqdrain(pp); n > 0 {
						lock(&sched.lock)
						globrunqputbatch(&drainQ, int32(n))
						unlock(&sched.lock)
					}
				}
				// Go back to draining, this time
				// without preemption.
				gcDrain(&pp.gcw, gcDrainFlushBgCredit)
			case gcMarkWorkerFractionalMode:
				gcDrain(&pp.gcw, gcDrainFractional|gcDrainUntilPreempt|gcDrainFlushBgCredit)
			case gcMarkWorkerIdleMode:
				gcDrain(&pp.gcw, gcDrainIdle|gcDrainUntilPreempt|gcDrainFlushBgCredit)
			}
			casgstatus(gp, _Gwaiting, _Grunning)
		})

		// 工作结束，统计时间
		// Account for time.
		duration := nanotime() - startTime
		switch pp.gcMarkWorkerMode {
		case gcMarkWorkerDedicatedMode:
			atomic.Xaddint64(&gcController.dedicatedMarkTime, duration)
			atomic.Xaddint64(&gcController.dedicatedMarkWorkersNeeded, 1)
		case gcMarkWorkerFractionalMode:
			atomic.Xaddint64(&gcController.fractionalMarkTime, duration)
			atomic.Xaddint64(&pp.gcFractionalMarkTime, duration)
		case gcMarkWorkerIdleMode:
			atomic.Xaddint64(&gcController.idleMarkTime, duration)
		}

		// 等待人数 +1，相当于自己干完了，就在等待来活，所有+1就相当于把自己放到等待队列
		// Was this the last worker and did we run out
		// of work?
		incnwait := atomic.Xadd(&work.nwait, +1)
		if incnwait > work.nproc {
			println("runtime: p.gcMarkWorkerMode=", pp.gcMarkWorkerMode,
				"work.nwait=", incnwait, "work.nproc=", work.nproc)
			throw("work.nwait > work.nproc")
		}

		// 去掉工种
		// We'll releasem after this point and thus this P may run
		// something else. We must clear the worker mode to avoid
		// attributing the mode to a different (non-worker) G in
		// traceGoStart.
		pp.gcMarkWorkerMode = gcMarkWorkerNotWorker

		// 如果所有工人都进入等待状态，且没有剩余工作。标记结束。
		// 有多少人等不重要，只要 work.nproc == work.nwait 就表示没有人在工作
		// If this worker reached a background mark completion
		// point, signal the main GC goroutine.
		if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
			// We don't need the P-local buffers here, allow
			// preemption becuse we may schedule like a regular
			// goroutine in gcMarkDone (block on locks, etc).
			releasem(node.m.ptr())
			node.m.set(nil)
			// 标记结束
			gcMarkDone()
		}
	}
}

// gcMarkWorkAvailable reports whether executing a mark worker
// on p is potentially useful. p may be nil, in which case it only
// checks the global sources of work.
func gcMarkWorkAvailable(p *p) bool {
	if p != nil && !p.gcw.empty() {
		return true
	}
	if !work.full.empty() {
		return true // global work available
	}
	if work.markrootNext < work.markrootJobs {
		return true // root scan work available
	}
	return false
}

// gcMark runs the mark (or, for concurrent GC, mark termination)
// All gcWork caches must be empty.
// STW is in effect at this point.
func gcMark(startTime int64) {
	if debug.allocfreetrace > 0 {
		tracegc()
	}

	if gcphase != _GCmarktermination {
		throw("in gcMark expecting to see gcphase as _GCmarktermination")
	}
	work.tstart = startTime

	// Check that there's no marking work remaining.
	if work.full != 0 || work.markrootNext < work.markrootJobs {
		print("runtime: full=", hex(work.full), " next=", work.markrootNext, " jobs=", work.markrootJobs, " nDataRoots=", work.nDataRoots, " nBSSRoots=", work.nBSSRoots, " nSpanRoots=", work.nSpanRoots, " nStackRoots=", work.nStackRoots, "\n")
		panic("non-empty mark queue after concurrent mark")
	}

	if debug.gccheckmark > 0 {
		// This is expensive when there's a large number of
		// Gs, so only do it if checkmark is also enabled.
		gcMarkRootCheck()
	}
	if work.full != 0 {
		throw("work.full != 0")
	}

	// Clear out buffers and double-check that all gcWork caches
	// are empty. This should be ensured by gcMarkDone before we
	// enter mark termination.
	//
	// TODO: We could clear out buffers just before mark if this
	// has a non-negligible impact on STW time.
	for _, p := range allp {
		// The write barrier may have buffered pointers since
		// the gcMarkDone barrier. However, since the barrier
		// ensured all reachable objects were marked, all of
		// these must be pointers to black objects. Hence we
		// can just discard the write barrier buffer.
		if debug.gccheckmark > 0 {
			// For debugging, flush the buffer and make
			// sure it really was all marked.
			wbBufFlush1(p)
		} else {
			p.wbBuf.reset()
		}

		gcw := &p.gcw
		if !gcw.empty() {
			printlock()
			print("runtime: P ", p.id, " flushedWork ", gcw.flushedWork)
			if gcw.wbuf1 == nil {
				print(" wbuf1=<nil>")
			} else {
				print(" wbuf1.n=", gcw.wbuf1.nobj)
			}
			if gcw.wbuf2 == nil {
				print(" wbuf2=<nil>")
			} else {
				print(" wbuf2.n=", gcw.wbuf2.nobj)
			}
			print("\n")
			throw("P has cached GC work at end of mark termination")
		}
		// There may still be cached empty buffers, which we
		// need to flush since we're going to free them. Also,
		// there may be non-zero stats because we allocated
		// black after the gcMarkDone barrier.
		gcw.dispose()
	}

	// Update the marked heap stat.
	gcController.heapMarked = work.bytesMarked

	// Flush scanAlloc from each mcache since we're about to modify
	// heapScan directly. If we were to flush this later, then scanAlloc
	// might have incorrect information.
	for _, p := range allp {
		c := p.mcache
		if c == nil {
			continue
		}
		gcController.heapScan += uint64(c.scanAlloc)
		c.scanAlloc = 0
	}

	// Update other GC heap size stats. This must happen after
	// cachestats (which flushes local statistics to these) and
	// flushallmcaches (which modifies gcController.heapLive).
	gcController.heapLive = work.bytesMarked
	gcController.heapScan = uint64(gcController.scanWork)

	if trace.enabled {
		traceHeapAlloc()
	}
}

// 默认情况下，采用后台并发模式执行清理操作
// gcSweep must be called on the system stack because it acquires the heap
// lock. See mheap for details.
//
// The world must be stopped.
//
//go:systemstack
func gcSweep(mode gcMode) {
	// 确认当前处于 STW
	assertWorldStopped()

	if gcphase != _GCoff {
		throw("gcSweep being done but phase is not GCoff")
	}

	lock(&mheap_.lock)
	// 累加代龄，重置状态
	mheap_.sweepgen += 2
	mheap_.sweepDrained = 0
	mheap_.pagesSwept = 0
	mheap_.sweepArenas = mheap_.allArenas
	mheap_.reclaimIndex = 0
	mheap_.reclaimCredit = 0
	unlock(&mheap_.lock)

	sweep.centralIndex.clear()

	// 同步阻塞模式
	if !_ConcurrentSweep || mode == gcForceBlockMode {
		// Special case synchronous sweep.
		// Record that no proportional sweeping has to happen.
		lock(&mheap_.lock)
		mheap_.sweepPagesPerByte = 0
		unlock(&mheap_.lock)
		// 清理所有 spans
		// Sweep all spans eagerly.
		for sweepone() != ^uintptr(0) {
			sweep.npausesweep++
		}
		// 释放队列
		// Free workbufs eagerly.
		prepareFreeWorkbufs()
		for freeSomeWbufs(false) {
		}
		// All "free" events for this mark/sweep cycle have
		// now happened, so we can make this profile cycle
		// available immediately.
		mProf_NextCycle()
		mProf_Flush()
		return
	}

	// 并发后台清理
	// Background sweep.
	lock(&sweep.lock)
	if sweep.parked {
		sweep.parked = false
		ready(sweep.g, 0, true)
	}
	unlock(&sweep.lock)
}

// gcResetMarkState resets global state prior to marking (concurrent
// or STW) and resets the stack scan state of all Gs.
//
// This is safe to do without the world stopped because any Gs created
// during or after this will start out in the reset state.
//
// gcResetMarkState must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack
func gcResetMarkState() {
	// This may be called during a concurrent phase, so lock to make sure
	// allgs doesn't change.
	forEachG(func(gp *g) {
		gp.gcscandone = false // set to true in gcphasework
		gp.gcAssistBytes = 0
	})

	// Clear page marks. This is just 1MB per 64GB of heap, so the
	// time here is pretty trivial.
	lock(&mheap_.lock)
	arenas := mheap_.allArenas
	unlock(&mheap_.lock)
	for _, ai := range arenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]
		for i := range ha.pageMarks {
			ha.pageMarks[i] = 0
		}
	}

	work.bytesMarked = 0
	work.initialHeapLive = atomic.Load64(&gcController.heapLive)
}

// Hooks for other packages

var poolcleanup func()

//go:linkname sync_runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func sync_runtime_registerPoolCleanup(f func()) {
	poolcleanup = f
}

func clearpools() {
	// clear sync.Pools
	if poolcleanup != nil {
		poolcleanup()
	}

	// Clear central sudog cache.
	// Leave per-P caches alone, they have strictly bounded size.
	// Disconnect cached list before dropping it on the floor,
	// so that a dangling ref to one entry does not pin all of them.
	lock(&sched.sudoglock)
	var sg, sgnext *sudog
	for sg = sched.sudogcache; sg != nil; sg = sgnext {
		sgnext = sg.next
		sg.next = nil
	}
	sched.sudogcache = nil
	unlock(&sched.sudoglock)

	// Clear central defer pools.
	// Leave per-P pools alone, they have strictly bounded size.
	lock(&sched.deferlock)
	for i := range sched.deferpool {
		// disconnect cached list before dropping it on the floor,
		// so that a dangling ref to one entry does not pin all of them.
		var d, dlink *_defer
		for d = sched.deferpool[i]; d != nil; d = dlink {
			dlink = d.link
			d.link = nil
		}
		sched.deferpool[i] = nil
	}
	unlock(&sched.deferlock)
}

// Timing

// itoaDiv formats val/(10**dec) into buf.
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	i := len(buf) - 1
	idec := i - dec
	for val >= 10 || i >= idec {
		buf[i] = byte(val%10 + '0')
		i--
		if i == idec {
			buf[i] = '.'
			i--
		}
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}

// fmtNSAsMS nicely formats ns nanoseconds as milliseconds.
func fmtNSAsMS(buf []byte, ns uint64) []byte {
	if ns >= 10e6 {
		// Format as whole milliseconds.
		return itoaDiv(buf, ns/1e6, 0)
	}
	// Format two digits of precision, with at most three decimal places.
	x := ns / 1e3
	if x == 0 {
		buf[0] = '0'
		return buf[:1]
	}
	dec := 3
	for x >= 100 {
		x /= 10
		dec--
	}
	return itoaDiv(buf, x, dec)
}

// Helpers for testing GC.

// gcTestMoveStackOnNextCall causes the stack to be moved on a call
// immediately following the call to this. It may not work correctly
// if any other work appears after this call (such as returning).
// Typically the following call should be marked go:noinline so it
// performs a stack check.
//
// In rare cases this may not cause the stack to move, specifically if
// there's a preemption between this call and the next.
func gcTestMoveStackOnNextCall() {
	gp := getg()
	gp.stackguard0 = stackForceMove
}

// gcTestIsReachable performs a GC and returns a bit set where bit i
// is set if ptrs[i] is reachable.
func gcTestIsReachable(ptrs ...unsafe.Pointer) (mask uint64) {
	// This takes the pointers as unsafe.Pointers in order to keep
	// them live long enough for us to attach specials. After
	// that, we drop our references to them.

	if len(ptrs) > 64 {
		panic("too many pointers for uint64 mask")
	}

	// Block GC while we attach specials and drop our references
	// to ptrs. Otherwise, if a GC is in progress, it could mark
	// them reachable via this function before we have a chance to
	// drop them.
	semacquire(&gcsema)

	// Create reachability specials for ptrs.
	specials := make([]*specialReachable, len(ptrs))
	for i, p := range ptrs {
		lock(&mheap_.speciallock)
		s := (*specialReachable)(mheap_.specialReachableAlloc.alloc())
		unlock(&mheap_.speciallock)
		s.special.kind = _KindSpecialReachable
		if !addspecial(p, &s.special) {
			throw("already have a reachable special (duplicate pointer?)")
		}
		specials[i] = s
		// Make sure we don't retain ptrs.
		ptrs[i] = nil
	}

	semrelease(&gcsema)

	// Force a full GC and sweep.
	GC()

	// Process specials.
	for i, s := range specials {
		if !s.done {
			printlock()
			println("runtime: object", i, "was not swept")
			throw("IsReachable failed")
		}
		if s.reachable {
			mask |= 1 << i
		}
		lock(&mheap_.speciallock)
		mheap_.specialReachableAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	}

	return mask
}

// gcTestPointerClass returns the category of what p points to, one of:
// "heap", "stack", "data", "bss", "other". This is useful for checking
// that a test is doing what it's intended to do.
//
// This is nosplit simply to avoid extra pointer shuffling that may
// complicate a test.
//
//go:nosplit
func gcTestPointerClass(p unsafe.Pointer) string {
	p2 := uintptr(noescape(p))
	gp := getg()
	if gp.stack.lo <= p2 && p2 < gp.stack.hi {
		return "stack"
	}
	if base, _, _ := findObject(p2, 0, 0); base != 0 {
		return "heap"
	}
	for _, datap := range activeModules() {
		if datap.data <= p2 && p2 < datap.edata || datap.noptrdata <= p2 && p2 < datap.enoptrdata {
			return "data"
		}
		if datap.bss <= p2 && p2 < datap.ebss || datap.noptrbss <= p2 && p2 <= datap.enoptrbss {
			return "bss"
		}
	}
	KeepAlive(p)
	return "other"
}
