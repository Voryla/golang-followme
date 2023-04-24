// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Lock-free stack.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// 全局队列采用了一种 Lock-Free Stack结构
// lfstack is the head of a lock-free stack.
//
// The zero value of lfstack is an empty list.
//
// This stack is intrusive. Nodes must embed lfnode as the first field.
//
// The stack does not keep GC-visible pointers to nodes, so the caller
// is responsible for ensuring the nodes are not garbage collected
// (typically by allocating them from manually-managed memory).
type lfstack uint64

// 无锁实现，使用循环确保原子操作成功，常见的无锁算法
func (head *lfstack) push(node *lfnode) {
	// 累加计数器，与地址共同生成位移流水号
	node.pushcnt++
	// 将node构造成新链表
	// 利用 地址+计数器 生成位移流水号，实现 Double-CAS 就能避开CAS问题
	new := lfstackPack(node, node.pushcnt)
	if node1 := lfstackUnpack(new); node1 != node {
		print("runtime: lfstack.push invalid packing: node=", node, " cnt=", hex(node.pushcnt), " packed=", hex(new), " -> node=", node1, "\n")
		throw("lfstack.push")
	}
	for {
		// 将原有链表挂到新 node上
		// 但并未修改原链表，所以CAS 保存不成功也不会影响

		// 如果 CAS 失败，那么新一轮循环重新 Load 和 CAS
		// 加上 CAS 会判断 old 是否相等，所以期间有其他并发操作也不影响
		old := atomic.Load64((*uint64)(head))
		node.next = old
		// 修改成功就退出，否则一直重试
		if atomic.Cas64((*uint64)(head), old, new) {
			break
		}
	}
}

func (head *lfstack) pop() unsafe.Pointer {
	for {
		// 获取头部节点
		old := atomic.Load64((*uint64)(head))
		if old == 0 {
			return nil
		}
		// 将 next 改为头部节点
		node := lfstackUnpack(old)
		next := atomic.Load64(&node.next)
		if atomic.Cas64((*uint64)(head), old, next) {
			return unsafe.Pointer(node)
		}
	}
}

func (head *lfstack) empty() bool {
	return atomic.Load64((*uint64)(head)) == 0
}

// lfnodeValidate panics if node is not a valid address for use with
// lfstack.push. This only needs to be called when node is allocated.
func lfnodeValidate(node *lfnode) {
	if lfstackUnpack(lfstackPack(node, ^uintptr(0))) != node {
		printlock()
		println("runtime: bad lfnode address", hex(uintptr(unsafe.Pointer(node))))
		throw("bad lfnode address")
	}
}
