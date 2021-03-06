// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"net/http"
)

// An Extension is a module of server-side logic that can attach
// itself to the header processing chains for incoming requests
// and outgoing respones.
type Extension interface {
	ReadRequest(req *http.Request, ext map[string]interface{}) error
	WriteResponse(resp *http.Response, ext map[string]interface{}) error
}

type extcfg struct {
	Name   string
	SubURL string
	Ext    Extension
}
