// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386
// +build amd64 386

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
// 可以看到在newg初始化时进行的一系列设置工作，将goexit先压入栈顶，然后伪造sp位置，
// 让cpu看起来是从goexit中调用的协程任务函数，然后将pc指针指向任务函数，当协程被执行时，
// 从pc处开始执行，任务函数执行完毕后执行goexit；
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	// newg的栈顶，目前new栈上只有fn函数的参数，
	// sp指向的是fn的第一个参数(参数 a,b,c 由于入栈顺序是从左至右故此处为第一个参数，也就是a最后入栈)
	sp := buf.sp
	// 为返回地址预留空间
	sp -= sys.PtrSize
	// buf.pc 中设置是goexit函数的第二条指令
	// 因为栈是自顶向下，先进后出的，所以这里伪装fn是被goexit函数调用的，goexit在前fn在后
	// 使得fn返回后到goexit继续执行，已完成一些清理工作
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	// 重新设置栈顶
	buf.sp = sp
	// 将pc指向goroutine的任务函数fn，这样当goroutine获得执行权时从函数入口开始执行
	// 如果书主协程，那么fn就是runtime.main从这里开始
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
