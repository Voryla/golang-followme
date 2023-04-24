// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !msan
// +build !msan

// Dummy MSan support API, used when not built with -msan.

package runtime

import (
	"unsafe"
)

// msanenabled 是 Golang 的一个内部变量，用于检测是否开启了 MSan（MemorySanitizer）内存检测器。
// MSan 是一种内存检测工具，可用于发现和定位 C/C++ 代码中的内存错误，包括使用未初始化的内存、溢出内存、使用已释放内存等。
// 在 Golang 的源代码中，msanenabled 变量通常用于帮助检测和修复 Golang 运行时的内存错误。
const msanenabled = false

// Because msanenabled is false, none of these functions should be called.

func msanread(addr unsafe.Pointer, sz uintptr)     { throw("msan") }
func msanwrite(addr unsafe.Pointer, sz uintptr)    { throw("msan") }
func msanmalloc(addr unsafe.Pointer, sz uintptr)   { throw("msan") }
func msanfree(addr unsafe.Pointer, sz uintptr)     { throw("msan") }
func msanmove(dst, src unsafe.Pointer, sz uintptr) { throw("msan") }
