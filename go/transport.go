// A minimal HTTP/1.1 server over raw TCP, built for this forge's workload.
//
// The dispatch is fully synchronous and every request is small, so net/http's
// machinery (Request/Response objects, header maps, chunked encoding) is pure
// overhead. This server parses one request at a time from the connection
// buffer, hands the complete request to the (synchronous) dispatch, and
// writes the response as a single buffer with an explicit Content-Length.
// Keep-alive and pipelining are supported; chunked request bodies are decoded
// (the suite sends Content-Length, but a client could in principle send
// chunked).
//
// Header parsing is targeted, not general: the dispatch only ever needs a
// handful of headers (authorization, content-length, connection,
// git-protocol, host, expect, transfer-encoding), so the parser walks the
// header lines once and picks those out by their first byte. Values are
// sub-slices of the connection buffer — no allocation, no copying.
//
// Responses take two forms. A structured reply is built here (status line +
// headers + body, one write). A pre-built Buffer is a complete response
// (status line, headers and body already joined) that goes straight to the
// socket with zero per-request work — the read workload's hot GET uses those.
// Pre-built replies are only ever produced for GET routes; HEAD requests get
// the structured form so the body can be withheld.

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
)

const maxHeadersBytes = 64 * 1024
const maxBodyBytes = 64 * 1024 * 1024
const maxChunks = 1 << 20 // chunked bodies: ~1M chunks max (a 64MiB body
// of 1-byte chunks would otherwise loop 64M times)

// Chunked trailers are discarded, but a client may not stream them forever.
const maxTrailerBytes = 8 * 1024

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// ---------------------------------------------------------------------------
// request ids
// ---------------------------------------------------------------------------

// The request-id pool: 64 ids, fixed for the life of the process, handed out
// round-robin. The contract requires a non-empty X-GitHub-Request-Id on every
// reply; it does not require uniqueness, and rotating ids keep consecutive
// replies distinct while letting hot paths pre-build whole responses (the id
// is baked into the bytes). Structured replies take the next id from the same
// pool, so ids rotate across both reply forms.
const requestIDPoolSize = 64

var requestIDs = func() []string {
	pidHex := strconv.FormatInt(int64(os.Getpid()), 16)
	ids := make([]string, requestIDPoolSize)
	for i := 0; i < requestIDPoolSize; i++ {
		ids[i] = fmt.Sprintf("req-%s-%08x", pidHex, i)
	}
	return ids
}()

var nextIDCounter int64

func nextRequestID() string {
	n := atomic.AddInt64(&nextIDCounter, 1) - 1
	return requestIDs[int(n)&(requestIDPoolSize-1)]
}

// ---------------------------------------------------------------------------
// header scanning
// ---------------------------------------------------------------------------

type headScan struct {
	method         []byte
	target         []byte
	version11      bool // true for HTTP/1.1, false for HTTP/1.0
	auth           []byte
	gitProtocol    []byte
	host           []byte
	connection     []byte
	expectContinue bool
	chunked        bool
	contentLength  int
	oversize       bool // Content-Length exceeded maxBodyBytes (413)
}

// asciiLower lowercases an ASCII byte.
func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// headerNameEq reports whether a raw header name (before the ':') equals a
// lowercase name, case-insensitively.
func headerNameEq(raw []byte, name string) bool {
	if len(raw) != len(name) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if asciiLower(raw[i]) != name[i] {
			return false
		}
	}
	return true
}

func trimHeaderValue(v []byte) []byte {
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t') {
		v = v[1:]
	}
	for len(v) > 0 && (v[len(v)-1] == ' ' || v[len(v)-1] == '\t') {
		v = v[:len(v)-1]
	}
	return v
}

// parseDecimal parses a decimal Content-Length. The second result is
// 0 when the value is valid, 1 when it is malformed, 2 when it exceeds
// maxBodyBytes (a 413, not a 400: the value is well-formed, just too big).
func parseDecimal(b []byte) (int, int) {
	if len(b) == 0 {
		return 0, 1
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, 1
		}
		n = n*10 + int(c-'0')
		if n > maxBodyBytes {
			return 0, 2
		}
	}
	return n, 0
}

// scanHead parses the request line + the headers the dispatch needs out of
// the head block (everything before the blank line). It returns false when
// the head is malformed.
func scanHead(head []byte, out *headScan) bool {
	// Request line.
	eol := bytes.Index(head, []byte("\r\n"))
	line := head
	if eol >= 0 {
		line = head[:eol]
	}
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return false
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return false
	}
	sp2 += sp1 + 1
	if sp2 == len(line)-1 {
		return false
	}
	method := line[:sp1]
	target := line[sp1+1 : sp2]
	version := line[sp2+1:]
	if len(target) == 0 {
		return false
	}
	// The request line itself must not contain a third space.
	if bytes.IndexByte(version, ' ') >= 0 {
		return false
	}
	var version11 bool
	if bytes.Equal(version, []byte("HTTP/1.1")) {
		version11 = true
	} else if !bytes.Equal(version, []byte("HTTP/1.0")) {
		return false
	}
	*out = headScan{method: method, target: target, version11: version11}

	// Header lines: one pass, dispatching on the first byte.
	rest := head
	if eol >= 0 {
		rest = head[eol+2:]
	} else {
		rest = head[len(head):]
	}
	for len(rest) > 0 {
		end := bytes.Index(rest, []byte("\r\n"))
		var hline []byte
		if end < 0 {
			hline, rest = rest, nil
		} else {
			hline, rest = rest[:end], rest[end+2:]
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		name := hline[:colon]
		value := trimHeaderValue(hline[colon+1:])
		switch asciiLower(name[0]) {
		case 'a':
			if headerNameEq(name, "authorization") {
				out.auth = value
			}
		case 'c':
			if headerNameEq(name, "content-length") {
				n, status := parseDecimal(value)
				if status == 2 {
					out.oversize = true
					return false
				}
				if status != 0 {
					return false
				}
				out.contentLength = n
			} else if headerNameEq(name, "connection") {
				out.connection = value
			}
		case 'e':
			if headerNameEq(name, "expect") {
				out.expectContinue = bytes.EqualFold(value, []byte("100-continue"))
			}
		case 'g':
			if headerNameEq(name, "git-protocol") {
				out.gitProtocol = value
			}
		case 'h':
			if headerNameEq(name, "host") {
				out.host = value
			}
		case 't':
			if headerNameEq(name, "transfer-encoding") {
				// "chunked" anywhere in the value, case-insensitively.
				lower := make([]byte, len(value))
				for i, c := range value {
					lower[i] = asciiLower(c)
				}
				out.chunked = bytes.Contains(lower, []byte("chunked"))
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// the server
// ---------------------------------------------------------------------------

var badRequestBytes = []byte("HTTP/1.1 400 Bad Request\r\ncontent-type: application/json\r\ncontent-length: 25\r\n\r\n{\"message\":\"Bad Request\"}")
var tooLargeBytes = []byte("HTTP/1.1 413 Payload Too Large\r\ncontent-type: application/json\r\ncontent-length: 31\r\n\r\n{\"message\":\"Payload Too Large\"}")
var continueBytes = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// Serve accepts connections on ln forever, serving each with dispatch.
func Serve(ln net.Listener, dispatch func(*Request) Reply) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go serveConn(conn, dispatch)
	}
}

type connState struct {
	conn   net.Conn
	dispatch func(*Request) Reply
	buf    []byte // buffered, unparsed bytes
	head   *headScan
	bodyLen int
	chunked bool
	chunks [][]byte
	chunkedBytes int
	chunkCount   int
	trailerBytes int
	chunkMode    int // 0=size, 1=data, 2=trailer
	chunkLeft    int
	closed       bool
}

const (
	chunkSize = iota
	chunkData
	chunkTrailer
)

func serveConn(conn net.Conn, dispatch func(*Request) Reply) {
	defer conn.Close()
	st := &connState{conn: conn, dispatch: dispatch, buf: make([]byte, 0, 32768)}
	tmp := make([]byte, 32768)
	for !st.closed {
		n, err := conn.Read(tmp)
		if n > 0 {
			st.buf = append(st.buf, tmp[:n]...)
			if !st.parseLoop() {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// parseLoop processes as many complete requests as are buffered. It returns
// false when the connection must close.
func (st *connState) parseLoop() bool {
	for {
		if st.head == nil {
			end := bytes.Index(st.buf, []byte("\r\n\r\n"))
			if end < 0 {
				if len(st.buf) > maxHeadersBytes {
					return false
				}
				return true
			}
			var parsed headScan
			if !scanHead(st.buf[:end], &parsed) {
				if parsed.oversize {
					st.conn.Write(tooLargeBytes)
				} else {
					st.conn.Write(badRequestBytes)
				}
				return false
			}
			st.buf = st.buf[end+4:]
			st.head = &parsed
			if parsed.expectContinue {
				st.conn.Write(continueBytes)
			}
			if parsed.chunked {
				st.chunked = true
				st.chunks = st.chunks[:0]
				st.chunkedBytes = 0
				st.chunkCount = 0
				st.trailerBytes = 0
				st.chunkMode = chunkSize
				st.chunkLeft = 0
			} else {
				st.bodyLen = parsed.contentLength
			}
		}

		if st.chunked {
			done, ok := st.consumeChunked()
			if !ok {
				return false
			}
			if !done {
				return true
			}
			var body []byte
			if len(st.chunks) > 0 {
				total := 0
				for _, c := range st.chunks {
					total += len(c)
				}
				body = make([]byte, 0, total)
				for _, c := range st.chunks {
					body = append(body, c...)
				}
			}
			current := st.head
			st.head = nil
			st.chunked = false
			if !st.respond(current, body) {
				return false
			}
			continue
		}

		if len(st.buf) < st.bodyLen {
			return true
		}
		var body []byte
		if st.bodyLen > 0 {
			body = st.buf[:st.bodyLen]
		}
		st.buf = st.buf[st.bodyLen:]
		current := st.head
		st.head = nil
		if !st.respond(current, body) {
			return false
		}
		// Loop: a pipelined next request may already be buffered.
	}
}

// consumeChunked consumes as much of the chunked body as is buffered.
// Returns (complete, connectionOK).
func (st *connState) consumeChunked() (bool, bool) {
	for {
		switch st.chunkMode {
		case chunkSize:
			eol := bytes.Index(st.buf, []byte("\r\n"))
			if eol < 0 {
				return false, true
			}
			sizeText := st.buf[:eol]
			if i := bytes.IndexByte(sizeText, ';'); i >= 0 {
				sizeText = sizeText[:i]
			}
			sizeText = bytes.TrimSpace(sizeText)
			size := 0
			for _, c := range sizeText {
				v := hexVal(c)
				if v < 0 {
					st.conn.Write(badRequestBytes)
					return false, false
				}
				size = size*16 + v
				if size > maxBodyBytes {
					return false, false
				}
			}
			st.buf = st.buf[eol+2:]
			if size == 0 {
				st.chunkMode = chunkTrailer
				continue
			}
			st.chunkedBytes += size
			if st.chunkedBytes > maxBodyBytes {
				return false, false
			}
			st.chunkCount++
			if st.chunkCount > maxChunks {
				return false, false
			}
			st.chunkLeft = size
			st.chunkMode = chunkData
		case chunkData:
			if len(st.buf) < st.chunkLeft+2 {
				return false, true
			}
			// The two bytes after chunk data must be CRLF; skipping
			// them unchecked would let a malformed stream desync the
			// parser.
			if st.buf[st.chunkLeft] != '\r' || st.buf[st.chunkLeft+1] != '\n' {
				st.conn.Write(badRequestBytes)
				return false, false
			}
			st.chunks = append(st.chunks, st.buf[:st.chunkLeft])
			st.buf = st.buf[st.chunkLeft+2:]
			st.chunkMode = chunkSize
		case chunkTrailer:
			eol := bytes.Index(st.buf, []byte("\r\n"))
			if eol < 0 {
				return false, true
			}
			if eol == 0 {
				st.buf = st.buf[2:]
				return true, true
			}
			st.trailerBytes += eol + 2
			if st.trailerBytes > maxTrailerBytes {
				return false, false
			}
			st.buf = st.buf[eol+2:]
		}
	}
}

func (st *connState) respond(current *headScan, body []byte) bool {
	target := current.target
	var queryString []byte
	if i := bytes.IndexByte(target, '?'); i >= 0 {
		queryString = target[i+1:]
	}
	var keepAlive bool
	if current.version11 {
		keepAlive = !bytes.EqualFold(current.connection, []byte("close"))
	} else {
		keepAlive = bytes.EqualFold(current.connection, []byte("keep-alive"))
	}
	req := &Request{
		Method:      current.method,
		Target:      target,
		QueryString: queryString,
		Auth:        current.auth,
		GitProtocol: current.gitProtocol,
		Host:        current.host,
		KeepAlive:   keepAlive,
		Body:        body,
	}
	result := st.dispatch(req)
	if result.Prebuilt != nil {
		// A complete pre-built response: one write, nothing to build.
		st.conn.Write(result.Prebuilt)
	} else {
		reason := reasonPhrases[result.Status]
		if reason == "" {
			reason = "OK"
		}
		var head []byte
		head = append(head, "HTTP/1.1 "...)
		head = strconv.AppendInt(head, int64(result.Status), 10)
		head = append(head, ' ')
		head = append(head, reason...)
		head = append(head, "\r\ncontent-type: "...)
		head = append(head, result.ContentType...)
		head = append(head, "\r\nx-github-request-id: "...)
		head = append(head, nextRequestID()...)
		head = append(head, "\r\n"...)
		for _, kv := range result.Extra {
			head = append(head, kv[0]...)
			head = append(head, ": "...)
			head = append(head, kv[1]...)
			head = append(head, "\r\n"...)
		}
		head = append(head, "content-length: "...)
		head = strconv.AppendInt(head, int64(len(result.Body)), 10)
		head = append(head, "\r\n\r\n"...)
		// HEAD responses carry the headers (and the would-be
		// Content-Length) but no body.
		if bytes.Equal(current.method, []byte("HEAD")) || len(result.Body) == 0 {
			st.conn.Write(head)
		} else {
			full := make([]byte, 0, len(head)+len(result.Body))
			full = append(full, head...)
			full = append(full, result.Body...)
			st.conn.Write(full)
		}
	}
	if !keepAlive {
		return false
	}
	// Reset the window when the buffer is drained, so a long-lived
	// keep-alive connection reuses one backing array instead of walking
	// it forward forever; shrink it when it grew past its useful size.
	if len(st.buf) == 0 {
		if cap(st.buf) > 65536 {
			st.buf = make([]byte, 0, 32768)
		} else {
			st.buf = st.buf[:0]
		}
	}
	return true
}
