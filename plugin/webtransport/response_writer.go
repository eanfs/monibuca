package webtransport

import (
	"bufio"
	"io"
	"net/http"
)

// ResponseWriter is a simple HTTP response writer for WebTransport
type ResponseWriter struct {
	header     http.Header
	statusCode int
	writer     *bufio.Writer
}

// NewResponseWriter creates a new ResponseWriter
func NewResponseWriter(w io.Writer) *ResponseWriter {
	return &ResponseWriter{
		header: make(http.Header),
		writer: bufio.NewWriter(w),
	}
}

// Header returns the header map that will be sent by WriteHeader
func (w *ResponseWriter) Header() http.Header {
	return w.header
}

// WriteHeader sends an HTTP response header with the provided status code
func (w *ResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Write writes the data to the connection as part of an HTTP reply
func (w *ResponseWriter) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

// Flush flushes the buffered data to the client
func (w *ResponseWriter) Flush() {
	w.writer.Flush()
}
