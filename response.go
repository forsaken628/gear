package gear

import (
	"bufio"
	"net"
	"net/http"
	"regexp"
	"strconv"
)

var defaultHeaderFilterReg = regexp.MustCompile(
	`(?i)^(accept|allow|retry-after|warning|vary|access-control-allow-|x-ratelimit-)`)

// Response wraps an http.ResponseWriter and implements its interface to be used
// by an HTTP handler to construct an HTTP response.
type Response struct {
	status      int    // response Status Code
	body        []byte // the response content.
	afterHooks  []func()
	endHooks    []func()
	ended       atomicBool // indicate that app middlewares run out.
	wroteHeader atomicBool
	w           http.ResponseWriter // the origin http.ResponseWriter, should not be override.
	rw          http.ResponseWriter // maybe a http.ResponseWriter wrapper
}

// Get gets the first value associated with the given key. If there are no values associated with the key, Get returns "". To access multiple values of a key, access the map directly with CanonicalHeaderKey.
func (r *Response) Get(key string) string {
	return r.Header().Get(key)
}

// Set sets the header entries associated with key to the single element value. It replaces any existing values associated with key.
func (r *Response) Set(key, value string) {
	r.Header().Set(key, value)
}

// Del deletes the header entries associated with key.
func (r *Response) Del(key string) {
	r.Header().Del(key)
}

// Vary manipulate the HTTP Vary header.
// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary
func (r *Response) Vary(field string) {
	if field != "" && r.Get(HeaderVary) != "*" {
		if field == "*" {
			r.Header().Set(HeaderVary, field)
		} else {
			r.Header().Add(HeaderVary, field)
		}
	}
}

// Status returns the current status code.
func (r *Response) Status() int {
	return r.status
}

// Type returns the current content type.
func (r *Response) Type() string {
	return r.Get(HeaderContentType)
}

// Body returns the response content. If you use Response.Write directly, the content will not be captured.
func (r *Response) Body() []byte {
	return r.body
}

// ResetHeader reset headers. If keepSubset is true,
// header matching `(?i)^(accept|allow|retry-after|warning|access-control-allow-)` will be keep
func (r *Response) ResetHeader(filterReg ...*regexp.Regexp) {
	reg := defaultHeaderFilterReg
	if len(filterReg) > 0 {
		reg = filterReg[0]
	}
	header := r.Header()
	for key := range header {
		if !reg.MatchString(key) {
			delete(header, key)
		}
	}
}

// Header returns the header map that will be sent by WriteHeader.
func (r *Response) Header() http.Header {
	return r.rw.Header()
}

// Write writes the data to the connection as part of an HTTP reply.
func (r *Response) Write(buf []byte) (int, error) {
	// Some http Handler will call Write directly.
	if !r.wroteHeader.isTrue() {
		if r.status == 0 {
			r.status = 200
		}
		r.WriteHeader(0)
	}
	return r.rw.Write(buf)
}

// WriteHeader sends an HTTP response header with status code.
// If WriteHeader is not called explicitly, the first call to Write
// will trigger an implicit WriteHeader(http.StatusOK).
// Thus explicit calls to WriteHeader are mainly used to send error codes.
func (r *Response) WriteHeader(code int) {
	if !r.wroteHeader.swapTrue() {
		return
	}
	// ensure that ended is true
	r.ended.setTrue()

	// set status before afterHooks
	if code > 0 {
		r.status = code
	}

	// execute "after hooks" with LIFO order before Response.WriteHeader
	runHooks(r.afterHooks)

	// check status, r.status maybe changed in afterHooks
	if !IsStatusCode(r.status) {
		if r.body != nil {
			r.status = http.StatusOK
		} else {
			// Misdirected request, http://tools.ietf.org/html/rfc7540#section-9.1.2
			// The request was directed at a server that is not able to produce a response.
			r.status = 421
		}
	} else if isEmptyStatus(r.status) {
		r.body = nil
	}

	// check and set Content-Length
	if r.body != nil {
		r.Set(HeaderContentLength, strconv.Itoa(len(r.body)))
	}
	r.rw.WriteHeader(r.status)
	// execute "end hooks" with LIFO order after Response.WriteHeader.
	// they run in a goroutine, in order to not block current process.
	if len(r.endHooks) > 0 {
		go runHooks(r.endHooks)
	}
}

// Flush implements the http.Flusher interface to allow an HTTP handler to flush
// buffered data to the client.
// See [http.Flusher](https://golang.org/pkg/net/http/#Flusher)
func (r *Response) Flush() {
	r.w.(http.Flusher).Flush()
}

// Hijack implements the http.Hijacker interface to allow an HTTP handler to
// take over the connection.
// See [http.Hijacker](https://golang.org/pkg/net/http/#Hijacker)
func (r *Response) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.w.(http.Hijacker).Hijack()
}

// CloseNotify implements the http.CloseNotifier interface to allow detecting
// when the underlying connection has gone away.
// This mechanism can be used to cancel long operations on the server if the
// client has disconnected before the response is ready.
// See [http.CloseNotifier](https://golang.org/pkg/net/http/#CloseNotifier)
func (r *Response) CloseNotify() <-chan bool {
	return r.w.(http.CloseNotifier).CloseNotify()
}

// Push implements http.Pusher.
// Example: https://github.com/teambition/gear/blob/master/example/http2/app.go
func (r *Response) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.w.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return Err.WithMsg("http.Pusher not implemented")
}

// HeaderWrote indecates that whether the reply header has been (logically) written.
func (r *Response) HeaderWrote() bool {
	return r.wroteHeader.isTrue()
}

func (r *Response) respond(status int, body []byte) (err error) {
	r.body = body
	r.WriteHeader(status)
	// body maybe reset to nil when WriteHeader.
	if r.body != nil {
		_, err = r.Write(r.body)
	}
	return
}

func runHooks(hooks []func()) {
	// run hooks in LIFO order
	for i := len(hooks) - 1; i >= 0; i-- {
		hooks[i]()
	}
}
