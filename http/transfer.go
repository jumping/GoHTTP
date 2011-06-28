// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bytes"
	"bufio"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
)

// transferWriter inspects the fields of a user-supplied Request or Response,
// sanitizes them without changing the user object and provides methods for
// writing the respective header, body and trailer in wire format.
type transferWriter struct {
	Body             io.Reader
	BodyCloser       io.Closer
	ResponseToHEAD   bool
	ContentLength    int64
	Close            bool
	TransferEncoding []string
	Trailer          Header
}

func newTransferWriter(r interface{}) (t *transferWriter, err os.Error) {
	t = &transferWriter{}

	// Extract relevant fields
	atLeastHTTP11 := false
	switch rr := r.(type) {
	case *Request:
		t.Body = rr.Body
		t.BodyCloser = rr.Body
		t.ContentLength = rr.ContentLength
		t.Close = rr.Close
		t.TransferEncoding = rr.TransferEncoding
		t.Trailer = rr.Trailer
		atLeastHTTP11 = rr.ProtoAtLeast(1, 1)
		if t.Body != nil && len(t.TransferEncoding) == 0 && atLeastHTTP11 {
			if t.ContentLength == 0 {
				// Test to see if it's actually zero or just unset.
				var buf [1]byte
				n, _ := io.ReadFull(t.Body, buf[:])
				if n == 1 {
					// Oh, guess there is data in this Body Reader after all.
					// The ContentLength field just wasn't set.
					// Stich the Body back together again, re-attaching our
					// consumed byte.
					t.ContentLength = -1
					t.Body = io.MultiReader(bytes.NewBuffer(buf[:]), t.Body)
				} else {
					// Body is actually empty.
					t.Body = nil
					t.BodyCloser = nil
				}
			}
			if t.ContentLength < 0 {
				t.TransferEncoding = []string{"chunked"}
			}
		}
	case *Response:
		t.Body = rr.Body
		t.BodyCloser = rr.Body
		t.ContentLength = rr.ContentLength
		t.Close = rr.Close
		t.TransferEncoding = rr.TransferEncoding
		t.Trailer = rr.Trailer
		atLeastHTTP11 = rr.ProtoAtLeast(1, 1)
		t.ResponseToHEAD = noBodyExpected(rr.Request.Method)
	}

	// Sanitize Body,ContentLength,TransferEncoding
	if t.ResponseToHEAD {
		t.Body = nil
		t.TransferEncoding = nil
		// ContentLength is expected to hold Content-Length
		if t.ContentLength < 0 {
			return nil, ErrMissingContentLength
		}
	} else {
		if !atLeastHTTP11 || t.Body == nil {
			t.TransferEncoding = nil
		}
		if chunked(t.TransferEncoding) {
			t.ContentLength = -1
		} else if t.Body == nil { // no chunking, no body
			t.ContentLength = 0
		}
	}

	// Sanitize Trailer
	if !chunked(t.TransferEncoding) {
		t.Trailer = nil
	}

	return t, nil
}

func noBodyExpected(requestMethod string) bool {
	return requestMethod == "HEAD"
}

func (t *transferWriter) WriteHeader(w io.Writer) (err os.Error) {
	if t.Close {
		_, err = io.WriteString(w, "Connection: close\r\n")
		if err != nil {
			return
		}
	}

	// Write Content-Length and/or Transfer-Encoding whose values are a
	// function of the sanitized field triple (Body, ContentLength,
	// TransferEncoding)
	if chunked(t.TransferEncoding) {
		_, err = io.WriteString(w, "Transfer-Encoding: chunked\r\n")
		if err != nil {
			return
		}
	} else if t.ContentLength > 0 || t.ResponseToHEAD || (t.ContentLength == 0 && isIdentity(t.TransferEncoding)) {
		io.WriteString(w, "Content-Length: ")
		_, err = io.WriteString(w, strconv.Itoa64(t.ContentLength)+"\r\n")
		if err != nil {
			return
		}
	}

	// Write Trailer header
	if t.Trailer != nil {
		// TODO: At some point, there should be a generic mechanism for
		// writing long headers, using HTTP line splitting
		io.WriteString(w, "Trailer: ")
		needComma := false
		for k := range t.Trailer {
			k = CanonicalHeaderKey(k)
			switch k {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				return &badStringError{"invalid Trailer key", k}
			}
			if needComma {
				io.WriteString(w, ",")
			}
			io.WriteString(w, k)
			needComma = true
		}
		_, err = io.WriteString(w, "\r\n")
	}

	return
}

func (t *transferWriter) WriteBody(w io.Writer) (err os.Error) {
	// Write body
	if t.Body != nil {
		if chunked(t.TransferEncoding) {
			cw := NewChunkedWriter(w)
			_, err = io.Copy(cw, t.Body)
			if err == nil {
				err = cw.Close()
			}
		} else if t.ContentLength == -1 {
			_, err = io.Copy(w, t.Body)
		} else {
			_, err = io.Copy(w, io.LimitReader(t.Body, t.ContentLength))
		}
		if err != nil {
			return err
		}
		if err = t.BodyCloser.Close(); err != nil {
			return err
		}
	}

	// TODO(petar): Place trailer writer code here.
	if chunked(t.TransferEncoding) {
		// Last chunk, empty trailer
		_, err = io.WriteString(w, "\r\n")
	}

	return
}

type transferReader struct {
	// Input
	Header        Header
	StatusCode    int
	RequestMethod string
	ProtoMajor    int
	ProtoMinor    int
	// Output
	Body             io.ReadCloser
	ContentLength    int64
	TransferEncoding []string
	Close            bool
	Trailer          Header
}

// bodyAllowedForStatus returns whether a given response status code
// permits a body.  See RFC2616, section 4.4.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}

// msg is *Request or *Response.
func readTransfer(msg interface{}, r *bufio.Reader) (err os.Error) {
	t := &transferReader{}

	// Unify input
	isResponse := false
	switch rr := msg.(type) {
	case *Response:
		t.Header = rr.Header
		t.StatusCode = rr.StatusCode
		t.RequestMethod = rr.Request.Method
		t.ProtoMajor = rr.ProtoMajor
		t.ProtoMinor = rr.ProtoMinor
		t.Close = shouldClose(t.ProtoMajor, t.ProtoMinor, t.Header)
		isResponse = true
	case *Request:
		t.Header = rr.Header
		t.ProtoMajor = rr.ProtoMajor
		t.ProtoMinor = rr.ProtoMinor
		// Transfer semantics for Requests are exactly like those for
		// Responses with status code 200, responding to a GET method
		t.StatusCode = 200
		t.RequestMethod = "GET"
	default:
		panic("unexpected type")
	}

	// Default to HTTP/1.1
	if t.ProtoMajor == 0 && t.ProtoMinor == 0 {
		t.ProtoMajor, t.ProtoMinor = 1, 1
	}

	// Transfer encoding, content length
	t.TransferEncoding, err = fixTransferEncoding(t.RequestMethod, t.Header)
	if err != nil {
		return err
	}

	t.ContentLength, err = fixLength(isResponse, t.StatusCode, t.RequestMethod, t.Header, t.TransferEncoding)
	if err != nil {
		return err
	}

	// Trailer
	t.Trailer, err = fixTrailer(t.Header, t.TransferEncoding)
	if err != nil {
		return err
	}

	// If there is no Content-Length or chunked Transfer-Encoding on a *Response
	// and the status is not 1xx, 204 or 304, then the body is unbounded.
	// See RFC2616, section 4.4.
	switch msg.(type) {
	case *Response:
		if t.ContentLength == -1 &&
			!chunked(t.TransferEncoding) &&
			bodyAllowedForStatus(t.StatusCode) {
			// Unbounded body.
			t.Close = true
		}
	}

	// Prepare body reader.  ContentLength < 0 means chunked encoding
	// or close connection when finished, since multipart is not supported yet
	switch {
	case chunked(t.TransferEncoding):
		t.Body = &body{Reader: newChunkedReader(r), hdr: msg, r: r, closing: t.Close}
	case t.ContentLength >= 0:
		// TODO: limit the Content-Length. This is an easy DoS vector.
		t.Body = &body{Reader: io.LimitReader(r, t.ContentLength), closing: t.Close}
	default:
		// t.ContentLength < 0, i.e. "Content-Length" not mentioned in header
		if t.Close {
			// Close semantics (i.e. HTTP/1.0)
			t.Body = &body{Reader: r, closing: t.Close}
		} else {
			// Persistent connection (i.e. HTTP/1.1)
			t.Body = &body{Reader: io.LimitReader(r, 0), closing: t.Close}
		}
	}

	// Unify output
	switch rr := msg.(type) {
	case *Request:
		rr.Body = t.Body
		rr.ContentLength = t.ContentLength
		rr.TransferEncoding = t.TransferEncoding
		rr.Close = t.Close
		rr.Trailer = t.Trailer
	case *Response:
		rr.Body = t.Body
		rr.ContentLength = t.ContentLength
		rr.TransferEncoding = t.TransferEncoding
		rr.Close = t.Close
		rr.Trailer = t.Trailer
	}

	return nil
}

// Checks whether chunked is part of the encodings stack
func chunked(te []string) bool { return len(te) > 0 && te[0] == "chunked" }

// Checks whether the encoding is explicitly "identity".
func isIdentity(te []string) bool { return len(te) == 1 && te[0] == "identity" }

// Sanitize transfer encoding
func fixTransferEncoding(requestMethod string, header Header) ([]string, os.Error) {
	raw, present := header["Transfer-Encoding"]
	if !present {
		return nil, nil
	}

	header["Transfer-Encoding"] = nil, false

	// Head responses have no bodies, so the transfer encoding
	// should be ignored.
	if requestMethod == "HEAD" {
		return nil, nil
	}

	encodings := strings.Split(raw[0], ",")
	te := make([]string, 0, len(encodings))
	// TODO: Even though we only support "identity" and "chunked"
	// encodings, the loop below is designed with foresight. One
	// invariant that must be maintained is that, if present,
	// chunked encoding must always come first.
	for _, encoding := range encodings {
		encoding = strings.ToLower(strings.TrimSpace(encoding))
		// "identity" encoding is not recored
		if encoding == "identity" {
			break
		}
		if encoding != "chunked" {
			return nil, &badStringError{"unsupported transfer encoding", encoding}
		}
		te = te[0 : len(te)+1]
		te[len(te)-1] = encoding
	}
	if len(te) > 1 {
		return nil, &badStringError{"too many transfer encodings", strings.Join(te, ",")}
	}
	if len(te) > 0 {
		// Chunked encoding trumps Content-Length. See RFC 2616
		// Section 4.4. Currently len(te) > 0 implies chunked
		// encoding.
		header["Content-Length"] = nil, false
		return te, nil
	}

	return nil, nil
}

// Determine the expected body length, using RFC 2616 Section 4.4. This
// function is not a method, because ultimately it should be shared by
// ReadResponse and ReadRequest.
func fixLength(isResponse bool, status int, requestMethod string, header Header, te []string) (int64, os.Error) {

	// Logic based on response type or status
	if noBodyExpected(requestMethod) {
		return 0, nil
	}
	if status/100 == 1 {
		return 0, nil
	}
	switch status {
	case 204, 304:
		return 0, nil
	}

	// Logic based on Transfer-Encoding
	if chunked(te) {
		return -1, nil
	}

	// Logic based on Content-Length
	cl := strings.TrimSpace(header.Get("Content-Length"))
	if cl != "" {
		n, err := strconv.Atoi64(cl)
		if err != nil || n < 0 {
			return -1, &badStringError{"bad Content-Length", cl}
		}
		return n, nil
	} else {
		header.Del("Content-Length")
	}

	if !isResponse && requestMethod == "GET" {
		// RFC 2616 doesn't explicitly permit nor forbid an
		// entity-body on a GET request so we permit one if
		// declared, but we default to 0 here (not -1 below)
		// if there's no mention of a body.
		return 0, nil
	}

	// Logic based on media type. The purpose of the following code is just
	// to detect whether the unsupported "multipart/byteranges" is being
	// used. A proper Content-Type parser is needed in the future.
	if strings.Contains(strings.ToLower(header.Get("Content-Type")), "multipart/byteranges") {
		return -1, ErrNotSupported
	}

	// Body-EOF logic based on other methods (like closing, or chunked coding)
	return -1, nil
}

// Determine whether to hang up after sending a request and body, or
// receiving a response and body
// 'header' is the request headers
func shouldClose(major, minor int, header Header) bool {
	if major < 1 {
		return true
	} else if major == 1 && minor == 0 {
		if !strings.Contains(strings.ToLower(header.Get("Connection")), "keep-alive") {
			return true
		}
		return false
	} else {
		// TODO: Should split on commas, toss surrounding white space,
		// and check each field.
		if strings.ToLower(header.Get("Connection")) == "close" {
			header.Del("Connection")
			return true
		}
	}
	return false
}

// Parse the trailer header
func fixTrailer(header Header, te []string) (Header, os.Error) {
	raw := header.Get("Trailer")
	if raw == "" {
		return nil, nil
	}

	header.Del("Trailer")
	trailer := make(Header)
	keys := strings.Split(raw, ",")
	for _, key := range keys {
		key = CanonicalHeaderKey(strings.TrimSpace(key))
		switch key {
		case "Transfer-Encoding", "Trailer", "Content-Length":
			return nil, &badStringError{"bad trailer key", key}
		}
		trailer.Del(key)
	}
	if len(trailer) == 0 {
		return nil, nil
	}
	if !chunked(te) {
		// Trailer and no chunking
		return nil, ErrUnexpectedTrailer
	}
	return trailer, nil
}

// body turns a Reader into a ReadCloser.
// Close ensures that the body has been fully read
// and then reads the trailer if necessary.
type body struct {
	io.Reader
	hdr     interface{}   // non-nil (Response or Request) value means read trailer
	r       *bufio.Reader // underlying wire-format reader for the trailer
	closing bool          // is the connection to be closed after reading body?
	closed  bool
}

// ErrBodyReadAfterClose is returned when reading a Request Body after
// the body has been closed. This typically happens when the body is
// read after an HTTP Handler calls WriteHeader or Write on its
// ResponseWriter.
var ErrBodyReadAfterClose = os.NewError("http: invalid Read on closed request Body")

func (b *body) Read(p []byte) (n int, err os.Error) {
	if b.closed {
		return 0, ErrBodyReadAfterClose
	}
	return b.Reader.Read(p)
}

func (b *body) Close() os.Error {
	if b.closed {
		return nil
	}
	defer func() {
		b.closed = true
	}()
	if b.hdr == nil && b.closing {
		// no trailer and closing the connection next.
		// no point in reading to EOF.
		return nil
	}

	if _, err := io.Copy(ioutil.Discard, b); err != nil {
		return err
	}

	if b.hdr == nil { // not reading trailer
		return nil
	}

	// TODO(petar): Put trailer reader code here

	return nil
}
