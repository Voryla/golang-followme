// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build freebsd && !arm
// +build freebsd,!arm

package runtime

func archauxv(tag, val uintptr) {
}
