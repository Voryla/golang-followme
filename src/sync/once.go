// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once 用来保证绝对一次执行的对象，例如在单例的初始化中使用。
// Once is an object that will perform exactly one action.
//
// A Once must not be copied after first use.
type Once struct {
	// done 表名某个动作是否被执行
	// 由于其使用频繁(热路径),故将其放在结构体的最上方
	// 热路径在每个调用点内进行内嵌
	// 将 done 放在第一位，在某些架构下(amd64/x86)能获得更加紧凑的指令，
	// 而在其他架构下能更少的指令(用于计算其偏移量)
	// done indicates whether the action has been performed.
	// It is first in the struct because it is used in the hot path.
	// The hot path is inlined at every call site.
	// Placing done first allows more compact instructions on some architectures (amd64/386),
	// and fewer instructions (to calculate offset) on other architectures.
	done uint32
	m    Mutex
}

// Do 当且仅当第一次调用时， f 会被执行，给定
// var once Once
// 如果 once.Do(f) 被多次调用则只在第一次回调用 f，即使每次提供的 f 不同。
// 每次执行必须新建一个 Once 实例。
// Do 用于变量的一次初始化，由于 f 是无参的，因此有必要使用函数字面量来捕获参数：
// 	config.once.Do(func() { config.init(filename) })
// 因为该调用无返回值，因此如果 f 调用了 Do， 则会导致死锁。
// 如果 f 发生 panic， 则 Do 认为 f 已经返回；之后的调用也不会调用 f 。
// Do calls the function f if and only if Do is being called for the
// first time for this instance of Once. In other words, given
// 	var once Once
// if once.Do(f) is called multiple times, only the first call will invoke f,
// even if f has a different value in each invocation. A new instance of
// Once is required for each function to execute.
//
// Do is intended for initialization that must be run exactly once. Since f
// is niladic, it may be necessary to use a function literal to capture the
// arguments to a function to be invoked by Do:
// 	config.once.Do(func() { config.init(filename) })
//
// Because no call to Do returns until the one call to f returns, if f causes
// Do to be called, it will deadlock.
//
// If f panics, Do considers it to have returned; future calls of Do return
// without calling f.
//
func (o *Once) Do(f func()) {
	// Note: Here is an incorrect implementation of Do:
	//
	//	if atomic.CompareAndSwapUint32(&o.done, 0, 1) {
	//		f()
	//	}
	//
	// Do guarantees that when it returns, f has finished.
	// This implementation would not implement that guarantee:
	// given two simultaneous calls, the winner of the cas would
	// call f, and the second would return immediately, without
	// waiting for the first's call to f to complete.
	// This is why the slow path falls back to a mutex, and why
	// the atomic.StoreUint32 must be delayed until after f returns.
	// fast-path:原子读取 Once 内部的 done 属性，是否为 0，是则进入慢速路径，否则调用结束，即有其他 G 执行了Do，done=1
	if atomic.LoadUint32(&o.done) == 0 {
		// Outlined slow-path to allow inlining of the fast-path.
		o.doSlow(f)
	}
}

func (o *Once) doSlow(f func()) {
	// slow-path 我们只使用了源自读取 o.done 的值，这是快速路径，但是并不保证在并发状态下，是不是有多个执行单元读取到了0，
	// 因此必须枷锁，这个操作相当昂贵，即 slow-path
	o.m.Lock()
	defer o.m.Unlock()
	// 加锁后读取到 done确实为0，即没有其他G执行到下面的代码，立即执行 f 并在结束时调用原子写，将 o.done 修改为1
	if o.done == 0 {
		defer atomic.StoreUint32(&o.done, 1)
		f()
	}
	// 当 o.done 为 0 的 goroutine 解锁后，其他人会继续加锁，这时会发现 o.done 已经为了 1 ，于是 f 已经不用在继续执行了
}
