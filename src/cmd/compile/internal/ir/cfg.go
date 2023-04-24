// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

var (
	// 定义显示声明的变量，在栈上能分配的最大容量(10MB)，若大于该值，显示变量将被分配在堆上
	// maximum size variable which we will allocate on the stack.
	// This limit is for explicit variable declarations like "var x T" or "x := ...".
	// Note: the flag smallframes can update this value.
	MaxStackVarSize = int64(10 * 1024 * 1024)

	// 定义隐式声明的变量，在栈上能够分配的最大容量(64KB),若大于改值，隐式变量将被分配在堆上
	// maximum size of implicit variables that we will allocate on the stack.
	//   p := new(T)          allocating T on the stack
	//   p := &T{}            allocating T on the stack
	//   s := make([]T, n)    allocating [n]T on the stack
	//   s := []byte("...")   allocating [n]byte on the stack
	// Note: the flag smallframes can update this value.
	MaxImplicitStackVarSize = int64(64 * 1024)

	//MaxSmallArraySize 是被认为很小的数组的最大大小。小数组将直接使用一系列常量存储进行初始化。大型数组将通过从静态临时复制来初始化。选择 256 字节以最小化生成的代码 + statictmp 大小。
	// MaxSmallArraySize is the maximum size of an array which is considered small.
	// Small arrays will be initialized directly with a sequence of constant stores.
	// Large arrays will be initialized by copying from a static temp.
	// 256 bytes was chosen to minimize generated code + statictmp size.
	MaxSmallArraySize = int64(256)
)
