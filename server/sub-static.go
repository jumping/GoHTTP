// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"path"
	"github.com/petar/GoHTTP/http"
)

// StaticSubserver is a Subserver that serves static files from a given directory.
type StaticSubserver struct {
	staticPath string
}

func NewStaticSubserver(staticPath string) *StaticSubserver {
	return &StaticSubserver{staticPath}
}

func (ss *StaticSubserver) Serve(q *Query) {
	req := q.GetRequest()
	if req.Method != "GET" {
		q.ContinueAndWrite(http.NewResponse404())
		return
	}
	p := q.GetPath()
	if len(p) == 0 {
		p = "index.html"
	} else if p[0] == '/' {
		p = p[1:]
	}
	full := path.Join(ss.staticPath, p)
	resp, _ := http.NewResponseFile(full)
	q.ContinueAndWrite(resp)
}