// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.staticlockranking
// +build !goexperiment.staticlockranking

package sync

import "unsafe"

// Approximation of notifyList in runtime/sema.go. Size and alignment must
// agree.
// notifyList 基于 ticket 实现一个通知列表
type notifyList struct {
	// wati 为下一个 waiter 的 ticket 编号，在没有 lock 的情况下原子自增
	wait   uint32
	// notify 是下一个被通知的 waiter 的 ticket 编号
	// 它可以在没有 lock 的情况下进行读取，但只有在持有lock的情况下才能进行写
	//
	// wait 和 notify 会产生 warp around 只要它们 "unwarapped" 的差别小于 2^31,这种情况可以被正确处理，对于 wrap around 的情况而言
	// 需要超过 2^31+ 个 goroutine 阻塞在相同的 condvar 上，这是不可能的
	notify uint32
	// waiter 列表
	lock   uintptr // key field of the mutex
	head   unsafe.Pointer
	tail   unsafe.Pointer
}
