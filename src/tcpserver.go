// tcpserver.go: hand-rolled HTTP/1.1 server (round 6).
//
// Replaces net/http's server machinery with a lean accept loop + request
// parser + ResponseWriter that dispatches to the SAME handlers
// (gitproto/rest/graphql/mcp) with byte-identical responses.
//
// What net/http did per request that this skips:
//   - ServeMux pattern matching (replaced by a switch; the redirect for
//     unclean paths is preserved, see dispatch)
//   - readRequest's allocation-heavy parsing (replaced by a tight parser
//     that builds a real *http.Request the handlers consume unchanged)
//   - response.w (bufio 2048) + chunkWriter + checkConnErrorWriter layers
//     (replaced by one 2048-byte body buffer + the conn bufio.Writer)
//   - per-request Date formatting (cached per second, like net/http's
//     server date cache)
//
// Wire fidelity: the header order and framing decisions replicate
// net/http's chunkWriter.writeHeader exactly (verified against the Go
// 1.25.4 stdlib source and by raw-byte diffs against the round-5 binary):
//   status line, handler headers sorted by canonical key, then
//   Date, Content-Length, Content-Type (only when sniffed), Connection,
//   Transfer-Encoding: chunked. Small bodies (<=2048 bytes at handler
//   return) get Content-Length; larger bodies get chunked framing with the
//   same 2048-byte buffer semantics. 204/304/1xx suppress Content-Length
//   and Transfer-Encoding; a handler-set Content-Type stays in sorted
//   position, a sniffed one goes after Content-Length.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bufferBeforeChunkingSize mirrors net/http's constant: bodies that fit
// entirely in this buffer at handler return are sent with Content-Length.
const bufferBeforeChunkingSize = 2048

// connIOSize is the bufio buffer for a connection (net/http uses 4 KiB;
// the bytes on the wire are identical either way, a larger buffer just
// means fewer write syscalls for large pack responses).
const connIOSize = 32 << 10

// --- per-second Date cache (net/http keeps one per server) ---

var (
	dateMu  sync.Mutex
	dateSec int64
	dateStr string
)

func currentDateString() string {
	now := time.Now().Unix()
	dateMu.Lock()
	defer dateMu.Unlock()
	if now != dateSec {
		dateSec = now
		dateStr = time.Unix(now, 0).UTC().Format(http.TimeFormat)
	}
	return dateStr
}

// --- pools ---

var (
	bufioReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, connIOSize) }}
	bufioWriterPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, connIOSize) }}
	headerPool      = sync.Pool{New: func() any { return make(http.Header, 8) }}
	requestPool     = sync.Pool{New: func() any { return &http.Request{} }}
	bodyBufPool     = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}
)

func getBodyBuf() *[]byte {
	b := bodyBufPool.Get().(*[]byte)
	*b = (*b)[:0]
	return b
}

func putBodyBuf(b *[]byte) {
	if cap(*b) <= 1<<20 {
		bodyBufPool.Put(b)
	}
}

// --- server ---

type tcpServer struct {
	srv *server
	ln  net.Listener
}

// serveTCP accepts connections until the listener closes. Each connection
// gets a goroutine, like net/http.
func serveTCP(ln net.Listener, srv *server) {
	ts := &tcpServer{srv: srv, ln: ln}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept error: %v", err)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(3 * time.Minute)
		}
		go ts.handleConn(conn)
	}
}

func (ts *tcpServer) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer bufioReaderPool.Put(br)
	bw := bufioWriterPool.Get().(*bufio.Writer)
	bw.Reset(conn)
	defer bufioWriterPool.Put(bw)

	for {
		req, body, closeAfter, err := ts.readRequest(conn, br, bw)
		if err != nil {
			if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
				// Malformed request: answer 400 like net/http, then close.
				// (The trial never sends malformed requests; this is only
				// a safety net for a stray probe.)
				_ = writeBadRequest(bw)
			}
			return
		}
		if req == nil {
			return
		}
		rw := newTCPResponseWriter(req, bw, closeAfter)
		ts.dispatch(rw, req)
		rw.finish()
		if body != nil {
			putBodyBuf(body)
		}
		// Return pooled request pieces.
		for k := range req.Header {
			delete(req.Header, k)
		}
		headerPool.Put(req.Header)
		requestPool.Put(req)
		if rw.closeAfterReply {
			return
		}
	}
}

func writeBadRequest(bw *bufio.Writer) error {
	bw.WriteString("HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: 16\r\n\r\n400 Bad Request\n")
	return bw.Flush()
}

// --- request parsing ---

var (
	errBadRequest  = errors.New("bad request")
	errLineTooLong = errors.New("request line too long")
)

// readRequest parses one request from br. It returns the *http.Request
// (pooled; the caller must return its Header and the request to the pools),
// the pooled body buffer (nil body means http.NoBody was used), whether the
// client asked to close, and an error. The body is read eagerly: handlers
// always ReadAll it, and an eager read keeps keep-alive safe. The 30 s
// read deadline (net/http's ReadHeaderTimeout) covers the request line +
// headers only; it is cleared once the headers are in.
func (ts *tcpServer) readRequest(conn net.Conn, br *bufio.Reader, bw *bufio.Writer) (*http.Request, *[]byte, bool, error) {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := br.ReadSlice('\n')
	if err != nil {
		return nil, nil, false, err
	}
	if len(line) > 64<<10 {
		return nil, nil, false, errLineTooLong
	}
	method, target, proto, ok := parseRequestLine(line)
	if !ok {
		return nil, nil, false, errBadRequest
	}
	major, minor, ok := parseProto(proto)
	if !ok {
		return nil, nil, false, errBadRequest
	}

	header := headerPool.Get().(http.Header)
	var host string
	var contentLength int64 = -1
	var chunked bool
	var expectContinue bool
	closeAfter := false
	seenCL := false
	// Headers.
	for {
		hline, err := br.ReadSlice('\n')
		if err != nil {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, err
		}
		if len(hline) > 64<<10 {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errLineTooLong
		}
		hline = trimCRLF(hline)
		if len(hline) == 0 {
			break
		}
		if hline[0] == ' ' || hline[0] == '\t' {
			// Obsolete line folding: net/http rejects it.
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errBadRequest
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errBadRequest
		}
		name := string(hline[:colon])
		value := strings.Trim(string(hline[colon+1:]), " \t")
		key := textproto.CanonicalMIMEHeaderKey(name)
		if !validHeaderFieldName(key) {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errBadRequest
		}
		header[key] = append(header[key], value)
		switch key {
		case "Host":
			host = value
		case "Content-Length":
			cl, err := strconv.ParseInt(value, 10, 64)
			if err != nil || cl < 0 {
				for k := range header {
					delete(header, k)
				}
				headerPool.Put(header)
				return nil, nil, false, errBadRequest
			}
			if seenCL && contentLength != cl {
				for k := range header {
					delete(header, k)
				}
				headerPool.Put(header)
				return nil, nil, false, errBadRequest
			}
			seenCL = true
			contentLength = cl
		case "Transfer-Encoding":
			// Chunked wins if it is the last coding (RFC 7230 3.3.1).
			for _, tok := range strings.Split(value, ",") {
				tok = strings.TrimSpace(strings.ToLower(tok))
				if tok == "chunked" {
					chunked = true
				} else if tok == "identity" {
					// no-op
				} else if tok != "" {
					for k := range header {
						delete(header, k)
					}
					headerPool.Put(header)
					return nil, nil, false, errBadRequest
				}
			}
		case "Expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		case "Connection":
			for _, tok := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "close") {
					closeAfter = true
				}
			}
		}
	}
	if major == 1 && minor == 1 && host == "" {
		// HTTP/1.1 requires Host.
		for k := range header {
			delete(header, k)
		}
		headerPool.Put(header)
		return nil, nil, false, errBadRequest
	}
	if major == 1 && minor == 0 {
		// HTTP/1.0: keep-alive only if explicitly requested.
		keepAlive := false
		for _, v := range header["Connection"] {
			for _, tok := range strings.Split(v, ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "keep-alive") {
					keepAlive = true
				}
			}
		}
		if !keepAlive {
			closeAfter = true
		}
	}

	// Request URI: origin-form (what the kit sends) or absolute-form.
	var u *url.URL
	var rawTarget string
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		parsed, err := url.ParseRequestURI(target)
		if err != nil {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errBadRequest
		}
		u = parsed
		rawTarget = target
		if host == "" {
			host = u.Host
		}
	} else {
		parsed, err := url.ParseRequestURI(target)
		if err != nil {
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, errBadRequest
		}
		u = parsed
		rawTarget = target
	}
	if u == nil || u.Path == "" {
		for k := range header {
			delete(header, k)
		}
		headerPool.Put(header)
		return nil, nil, false, errBadRequest
	}

	// Body.
	var bodyBuf *[]byte
	var bodyReader io.ReadCloser = http.NoBody
	cl := contentLength
	if chunked {
		cl = -1
	}
	if chunked || cl > 0 {
		// Headers are in: the header deadline no longer applies.
		_ = conn.SetReadDeadline(time.Time{})
		if expectContinue {
			// Send 100 Continue before reading the body, like net/http
			// does on the first Body.Read.
			bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
			_ = bw.Flush()
		}
		buf := getBodyBuf()
		var rerr error
		if chunked {
			rerr = readChunkedBody(br, buf)
		} else {
			rerr = readFixedBody(br, buf, cl)
		}
		if rerr != nil {
			putBodyBuf(buf)
			for k := range header {
				delete(header, k)
			}
			headerPool.Put(header)
			return nil, nil, false, rerr
		}
		bodyBuf = buf
		bodyReader = io.NopCloser(bytes.NewReader(*buf))
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}

	req := requestPool.Get().(*http.Request)
	*req = http.Request{
		Method:        method,
		URL:           u,
		Proto:         proto,
		ProtoMajor:    major,
		ProtoMinor:    minor,
		Header:        header,
		Body:          bodyReader,
		ContentLength: cl,
		Host:          host,
		RemoteAddr:    conn.RemoteAddr().String(),
		RequestURI:    rawTarget,
		TLS:           nil,
		Close:         closeAfter,
	}
	if chunked {
		req.TransferEncoding = []string{"chunked"}
	}
	return req, bodyBuf, closeAfter, nil
}

func parseRequestLine(line []byte) (method, target, proto string, ok bool) {
	line = trimCRLF(line)
	// METHOD SP TARGET SP PROTO
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return "", "", "", false
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 <= 0 {
		return "", "", "", false
	}
	method = string(line[:sp1])
	target = string(line[sp1+1 : sp1+1+sp2])
	proto = string(line[sp1+1+sp2+1:])
	if method == "" || target == "" || proto == "" {
		return "", "", "", false
	}
	for i := 0; i < len(method); i++ {
		if !isTokenChar(method[i]) {
			return "", "", "", false
		}
	}
	return method, target, proto, true
}

func parseProto(proto string) (major, minor int, ok bool) {
	if proto == "HTTP/1.1" {
		return 1, 1, true
	}
	if proto == "HTTP/1.0" {
		return 1, 0, true
	}
	return 0, 0, false
}

func trimCRLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

func isTokenChar(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
		return false
	}
	return true
}

func validHeaderFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTokenChar(s[i]) {
			return false
		}
	}
	return true
}

func readFixedBody(br *bufio.Reader, buf *[]byte, n int64) error {
	if n == 0 {
		return nil
	}
	if int64(cap(*buf)) < n {
		*buf = make([]byte, 0, n)
	}
	*buf = (*buf)[:n]
	_, err := io.ReadFull(br, *buf)
	return err
}

func readChunkedBody(br *bufio.Reader, buf *[]byte) error {
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return err
		}
		line = trimCRLF(line)
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(line)), 16, 64)
		if err != nil || size < 0 {
			return errBadRequest
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				tline, err := br.ReadSlice('\n')
				if err != nil {
					return err
				}
				if len(trimCRLF(tline)) == 0 {
					return nil
				}
			}
		}
		start := len(*buf)
		if cap(*buf)-len(*buf) < int(size) {
			nb := make([]byte, len(*buf), len(*buf)+int(size))
			copy(nb, *buf)
			*buf = nb
		}
		*buf = (*buf)[:start+int(size)]
		if _, err := io.ReadFull(br, (*buf)[start:]); err != nil {
			return err
		}
		// Trailing CRLF after the chunk.
		if _, err := br.ReadSlice('\n'); err != nil {
			return err
		}
	}
}

// --- ResponseWriter ---

type tcpResponseWriter struct {
	bw              *bufio.Writer
	req             *http.Request
	header          http.Header
	status          int
	wroteHeader     bool // WriteHeader called (status recorded)
	headersSent     bool // wire headers flushed
	chunking        bool
	closeAfterReply bool
	contentLength   int64 // -1 = unknown, >= 0 = declared (explicit or auto)
	buf             [bufferBeforeChunkingSize]byte
	bufLen          int
	wrote           int64
}

func newTCPResponseWriter(req *http.Request, bw *bufio.Writer, reqClose bool) *tcpResponseWriter {
	w := &tcpResponseWriter{
		bw:              bw,
		req:             req,
		header:          make(http.Header, 4),
		status:          0,
		contentLength:   -1,
		closeAfterReply: reqClose,
	}
	return w
}

func (w *tcpResponseWriter) Header() http.Header { return w.header }

func (w *tcpResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return // net/http logs "superfluous WriteHeader"; we just ignore it
	}
	w.wroteHeader = true
	w.status = code
	if cl := w.header.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v >= 0 {
			w.contentLength = v
		} else {
			w.header.Del("Content-Length")
		}
	}
}

func (w *tcpResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !bodyAllowedForStatus(w.status) {
		return len(p), nil
	}
	n := len(p)
	for len(p) > 0 {
		if w.bufLen == 0 && len(p) >= bufferBeforeChunkingSize {
			// Large write bypasses the buffer: one chunk, like net/http's bufio.
			w.flushChunk(p)
			p = nil
		} else {
			k := copy(w.buf[w.bufLen:], p)
			w.bufLen += k
			p = p[k:]
			if w.bufLen == bufferBeforeChunkingSize {
				w.flushChunk(w.buf[:w.bufLen])
				w.bufLen = 0
			}
		}
	}
	return n, nil
}

// flushChunk writes the headers (first time) and one chunk of data.
func (w *tcpResponseWriter) flushChunk(data []byte) {
	if !w.headersSent {
		w.writeHeader(data, false)
	}
	if w.req.Method == http.MethodHead {
		return // HEAD: headers only, body discarded
	}
	if w.chunking {
		w.bw.WriteString(strconv.FormatInt(int64(len(data)), 16))
		w.bw.WriteString("\r\n")
		w.bw.Write(data)
		w.bw.WriteString("\r\n")
	} else {
		// Content-Length was declared (explicit): write the body raw.
		w.bw.Write(data)
	}
	w.wrote += int64(len(data))
}

// finish flushes the remaining buffered body and the terminating chunk,
// mirroring net/http's finishRequest.
func (w *tcpResponseWriter) finish() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.headersSent {
		w.writeHeader(w.buf[:w.bufLen], true)
		if bodyAllowedForStatus(w.status) && w.req.Method != http.MethodHead && w.bufLen > 0 {
			w.bw.Write(w.buf[:w.bufLen])
			w.wrote += int64(w.bufLen)
		}
	} else if w.chunking {
		if w.bufLen > 0 && w.req.Method != http.MethodHead {
			w.bw.WriteString(strconv.FormatInt(int64(w.bufLen), 16))
			w.bw.WriteString("\r\n")
			w.bw.Write(w.buf[:w.bufLen])
			w.bw.WriteString("\r\n")
			w.wrote += int64(w.bufLen)
		}
		w.bw.WriteString("0\r\n\r\n")
	} else if w.bufLen > 0 && w.req.Method != http.MethodHead {
		w.bw.Write(w.buf[:w.bufLen])
		w.wrote += int64(w.bufLen)
	}
	w.bufLen = 0
	_ = w.bw.Flush()
}

// writeHeader finalizes and writes the response headers. p is the first
// body bytes (used for Content-Type sniffing and the auto Content-Length).
// handlerDone is true only when called from finish().
func (w *tcpResponseWriter) writeHeader(p []byte, handlerDone bool) {
	if w.headersSent {
		return
	}
	w.headersSent = true
	isHEAD := w.req.Method == http.MethodHead
	code := w.status

	// 1xx (except 101): status line + headers + CRLF, no body framing.
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		writeStatusLine(w.bw, w.req.ProtoAtLeast(1, 1), code)
		writeHeaderSubset(w.bw, w.header, nil)
		w.bw.WriteString("\r\n")
		_ = w.bw.Flush()
		return
	}

	header := w.header
	// Suppress Content-Length / Transfer-Encoding for bodies that must
	// not have one (204, 304, 1xx).
	if !bodyAllowedForStatus(code) {
		header.Del("Content-Length")
		header.Del("Transfer-Encoding")
	}

	te := header.Get("Transfer-Encoding")
	hasTE := te != ""
	hasCL := w.contentLength != -1

	// Auto Content-Length when the handler is done and the whole body
	// is in hand (the common case: small responses).
	var autoCL []byte
	if handlerDone && !hasTE && bodyAllowedForStatus(code) && !headerHas(header, "Content-Length") && (!isHEAD || len(p) > 0) {
		w.contentLength = int64(len(p))
		autoCL = strconv.AppendInt(nil, int64(len(p)), 10)
		hasCL = true
	}

	// HTTP/1.0 keep-alive: echo "Connection: keep-alive".
	var connHeader string
	if w.req.ProtoMajor == 1 && w.req.ProtoMinor == 0 && !w.closeAfterReply && (isHEAD || hasCL || !bodyAllowedForStatus(code)) {
		if _, ok := header["Connection"]; !ok {
			connHeader = "keep-alive"
		}
	}
	if !w.req.ProtoAtLeast(1, 1) || w.req.Close {
		w.closeAfterReply = true
	}
	if header.Get("Connection") == "close" {
		w.closeAfterReply = true
	}

	// Sniff Content-Type from the first body bytes when unset.
	var sniffedCT string
	if bodyAllowedForStatus(code) {
		if _, haveType := header["Content-Type"]; !haveType {
			if ce := header.Get("Content-Encoding"); ce == "" && !hasTE && len(p) > 0 {
				sniffedCT = http.DetectContentType(p)
			}
		}
	}

	// Date.
	var date string
	if _, ok := header["Date"]; !ok {
		date = currentDateString()
	}

	// Content-Length + Transfer-Encoding conflict: CL loses (net/http logs).
	if hasCL && hasTE && te != "identity" {
		header.Del("Content-Length")
		hasCL = false
		w.contentLength = -1
		autoCL = nil
	}

	// Chunking decision.
	var transferEncoding string
	if isHEAD || !bodyAllowedForStatus(code) || code == http.StatusNoContent {
		header.Del("Transfer-Encoding")
	} else if hasCL {
		header.Del("Transfer-Encoding")
	} else if w.req.ProtoAtLeast(1, 1) {
		if hasTE && te == "identity" {
			w.chunking = false
			w.closeAfterReply = true
			header.Del("Transfer-Encoding")
		} else {
			w.chunking = true
			transferEncoding = "chunked"
			if hasTE && te == "chunked" {
				header.Del("Transfer-Encoding")
			}
		}
	} else {
		w.closeAfterReply = true
		header.Del("Transfer-Encoding")
	}
	if w.chunking {
		header.Del("Content-Length")
		autoCL = nil
	}

	// Connection header on the wire.
	if w.closeAfterReply && !hasToken(header.Get("Connection"), "close") {
		header.Del("Connection")
		if w.req.ProtoAtLeast(1, 1) {
			connHeader = "close"
		}
	}

	writeStatusLine(w.bw, w.req.ProtoAtLeast(1, 1), code)
	writeHeaderSubset(w.bw, header, nil)
	// extraHeader order: Date, Content-Length, Content-Type, Connection, Transfer-Encoding.
	if date != "" {
		w.bw.WriteString("Date: " + date + "\r\n")
	}
	if autoCL != nil {
		w.bw.WriteString("Content-Length: " + string(autoCL) + "\r\n")
	}
	if sniffedCT != "" {
		w.bw.WriteString("Content-Type: " + sniffedCT + "\r\n")
	}
	if connHeader != "" {
		w.bw.WriteString("Connection: " + connHeader + "\r\n")
	}
	if transferEncoding != "" {
		w.bw.WriteString("Transfer-Encoding: " + transferEncoding + "\r\n")
	}
	w.bw.WriteString("\r\n")
}

func headerHas(h http.Header, key string) bool {
	_, ok := h[textproto.CanonicalMIMEHeaderKey(key)]
	return ok
}

func hasToken(v, token string) bool {
	for _, t := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(t), token) {
			return true
		}
	}
	return false
}

func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent:
		return false
	case status == http.StatusNotModified:
		return false
	}
	return true
}

func writeStatusLine(bw *bufio.Writer, is11 bool, code int) {
	if is11 {
		bw.WriteString("HTTP/1.1 ")
	} else {
		bw.WriteString("HTTP/1.0 ")
	}
	if text := http.StatusText(code); text != "" {
		bw.WriteString(strconv.Itoa(code))
		bw.WriteString(" ")
		bw.WriteString(text)
		bw.WriteString("\r\n")
	} else {
		fmt.Fprintf(bw, "%03d status code %d\r\n", code, code)
	}
}

// writeHeaderSubset writes the handler headers sorted by canonical key,
// one line per value, dropping invalid names (net/http's WriteSubset).
func writeHeaderSubset(bw *bufio.Writer, h http.Header, exclude map[string]bool) {
	keys := make([]string, 0, len(h))
	for k := range h {
		if exclude != nil && exclude[k] {
			continue
		}
		keys = append(keys, k)
	}
	// Sort (few keys; insertion sort is fine, but sort.Strings is clearer).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for _, k := range keys {
		if !validHeaderFieldName(k) {
			continue
		}
		for _, v := range h[k] {
			v = strings.ReplaceAll(v, "\n", " ")
			v = strings.ReplaceAll(v, "\r", " ")
			v = strings.Trim(v, " \t")
			bw.WriteString(k)
			bw.WriteString(": ")
			bw.WriteString(v)
			bw.WriteString("\r\n")
		}
	}
}

// --- routing (ServeMux-faithful) ---

// cleanPath is net/http's cleanPath (stdlib): canonical path for p,
// eliminating . and .. elements, restoring a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

func (ts *tcpServer) dispatch(w http.ResponseWriter, r *http.Request) {
	// ServeMux (Go 1.25) cleans the ESCAPED path, not the decoded one:
	// path := r.URL.EscapedPath(); path = cleanPath(path); redirect iff
	// the cleaned escaped path differs. %XX sequences are NOT decoded
	// for this check, so /%61daiversen does NOT redirect (the handler
	// later sees the decoded path). Only dot segments and duplicate
	// slashes trigger the redirect.
	escaped := r.URL.EscapedPath()
	if cleaned := cleanPath(escaped); cleaned != escaped {
		u := &url.URL{Path: cleaned, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/healthz":
		healthzHandler(w, r)
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		ts.srv.restHandler(w, r)
	case path == "/api/graphql":
		ts.srv.graphqlHandler(w, r)
	case path == "/mcp":
		ts.srv.mcpHandler(w, r)
	default:
		// main.go's "/" handler: only /{owner}/{repo}.git/* reaches git.
		p := strings.TrimPrefix(path, "/")
		parts := strings.SplitN(p, "/", 3)
		if len(parts) >= 2 && strings.HasSuffix(parts[1], ".git") {
			ts.srv.store.gitHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", ts.srv.nextRequestID())
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}
