// Package httpenc provides a handler to encode a response content.
package httpenc

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/kechako/httpqv"
)

type EncodingType string

const (
	Gzip    EncodingType = "gzip"
	Deflate EncodingType = "deflate"
	Brotli  EncodingType = "br"
)

func (typ EncodingType) IsValid() bool {
	switch typ {
	case Gzip, Deflate, Brotli:
		return true
	}
	return false
}

var precompressionEncodeMap = map[string]EncodingType{
	".gz": Gzip,
	".br": Brotli,
}

const (
	contentTypeHeader     = "Content-Type"
	contentEncodingHeader = "Content-Encoding"
)

// Handler returns a handler that encodes a response content.
func Handler(next http.Handler, opts ...Option) http.Handler {
	options := &handlerOptions{
		gzipLevel:    gzip.DefaultCompression,
		deflateLevel: zlib.DefaultCompression,
		brotliLevel:  brotli.DefaultCompression,
	}
	for _, opt := range opts {
		opt.apply(options)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		// supported headers
		case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions, http.MethodPatch:
		default:
			next.ServeHTTP(w, r)
			return
		}

		name := path.Base(r.URL.Path)
		ext := path.Ext(name)

		values := parseAcceptedEncoding(r)
		accepted := map[string]*httpqv.Value{}
		for _, v := range values {
			accepted[v.Value] = v
		}

		newRW := w
		if enc, ok := precompressionEncodeMap[ext]; ok {
			header := http.Header{}

			origExt := path.Ext(name[:len(name)-len(ext)])
			header.Set(contentTypeHeader, contentTypeByExtension(origExt))

			if _, ok := accepted[string(enc)]; ok {
				// It jsut write the precompression content.
				// And set Content-Encoding header for it.
				header.Set(contentEncodingHeader, string(enc))
				hw := newHeaderResponseWriter(w, header)
				defer hw.Close()

				newRW = hw
			} else {
				// Precompression content is requested, but the client does not accept the content encoding.
				// Therefore, it decode the precompression content.
				dw := newDecodeResonseWriter(w, enc, header)
				defer dw.Close()

				newRW = dw
			}
		} else {
			for _, value := range values {
				enc := EncodingType(value.Value)
				if enc.IsValid() {
					ew := newEncodeResonseWriter(w, enc, options)
					defer ew.Close()

					newRW = ew
					break
				}
			}
		}

		next.ServeHTTP(newRW, r)
	})
}

func parseAcceptedEncoding(r *http.Request) []*httpqv.Value {
	s := r.Header.Get("Accept-Encoding")
	if s == "" {
		return nil
	}

	values, err := httpqv.Parse(s)
	if err != nil {
		return nil
	}

	httpqv.Sort(values)

	return values
}

func contentTypeByExtension(ext string) string {
	typ := mime.TypeByExtension(ext)
	if typ == "" {
		typ = "application/octet-stream"
	}
	return typ
}

type encodeResponseWriter struct {
	w           http.ResponseWriter
	typ         EncodingType
	enc         io.WriteCloser
	wroteHeader bool
}

var (
	_ http.ResponseWriter = (*encodeResponseWriter)(nil)
)

func newEncodeResonseWriter(w http.ResponseWriter, typ EncodingType, options *handlerOptions) *encodeResponseWriter {
	var enc io.WriteCloser
	switch typ {
	case Gzip:
		enc, _ = gzip.NewWriterLevel(w, options.gzipLevel)
	case Deflate:
		enc, _ = zlib.NewWriterLevel(w, options.deflateLevel)
	case Brotli:
		enc = brotli.NewWriterLevel(w, options.brotliLevel)
	}

	return &encodeResponseWriter{
		w:   w,
		typ: typ,
		enc: enc,
	}
}

func (w *encodeResponseWriter) Close() error {
	return w.enc.Close()
}

func (w *encodeResponseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *encodeResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.enc.Write(b)
}

func (w *encodeResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	if contentLength := w.Header().Get("Content-Length"); contentLength != "" {
		w.Header().Del("Content-Length")
	}

	w.Header().Set(contentEncodingHeader, string(w.typ))

	w.w.WriteHeader(statusCode)
}

type decodeResponseWriter struct {
	w           http.ResponseWriter
	typ         EncodingType
	header      http.Header
	wroteHeader bool

	pr   *io.PipeReader
	pw   *io.PipeWriter
	once sync.Once

	wg   sync.WaitGroup
	exit chan struct{}
}

var (
	_ http.ResponseWriter = (*decodeResponseWriter)(nil)
)

func newDecodeResonseWriter(w http.ResponseWriter, typ EncodingType, header http.Header) *decodeResponseWriter {
	pr, pw := io.Pipe()

	return &decodeResponseWriter{
		w:      w,
		typ:    typ,
		header: header,
		pr:     pr,
		pw:     pw,
	}
}

func (w *decodeResponseWriter) Close() error {
	defer w.wg.Wait()

	return w.pw.Close()
}

func (w *decodeResponseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *decodeResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	w.once.Do(func() {
		w.wg.Add(1)
		go w.write()
	})

	n, err := w.pw.Write(b)
	if err != nil {
		return 0, fmt.Errorf("httpenc: failed to decode %s: %w", w.typ, err)
	}

	return n, nil
}

func (w *decodeResponseWriter) write() {
	defer w.wg.Done()
	defer w.pr.Close()

	var dec io.ReadCloser
	switch w.typ {
	case Gzip:
		r, err := gzip.NewReader(w.pr)
		if err != nil {
			err := fmt.Errorf("httpenc: failed to create gzip.Reader: %w", err)
			w.pr.CloseWithError(err)
			return
		}
		dec = r
	case Deflate:
		r, err := zlib.NewReader(w.pr)
		if err != nil {
			err := fmt.Errorf("httpenc: failed to create zlib.Reader: %w", err)
			w.pr.CloseWithError(err)
			return
		}
		dec = r
	case Brotli:
		dec = io.NopCloser(brotli.NewReader(w.pr))
	}
	defer dec.Close()

	_, err := io.Copy(w.w, dec)
	if err != nil && err != io.EOF {
		w.pr.CloseWithError(err)
		return
	}
}

func (w *decodeResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	for key, values := range w.header {
		w.Header()[key] = values
	}

	if contentLength := w.Header().Get("Content-Length"); contentLength != "" {
		w.Header().Del("Content-Length")
	}

	w.Header().Del(contentEncodingHeader)

	w.w.WriteHeader(statusCode)
}

type headerResponseWriter struct {
	w           http.ResponseWriter
	header      http.Header
	wroteHeader bool
}

var (
	_ http.ResponseWriter = (*headerResponseWriter)(nil)
)

func newHeaderResponseWriter(w http.ResponseWriter, header http.Header) *headerResponseWriter {

	return &headerResponseWriter{
		w:      w,
		header: header,
	}
}

func (w *headerResponseWriter) Close() error {
	return nil
}

func (w *headerResponseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *headerResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.w.Write(b)
}

func (w *headerResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	for key, values := range w.header {
		w.Header()[key] = values
	}

	w.w.WriteHeader(statusCode)
}

type handlerOptions struct {
	gzipLevel    int
	deflateLevel int
	brotliLevel  int
}

type Option interface {
	apply(opts *handlerOptions)
}

type optionFunc func(opts *handlerOptions)

func (f optionFunc) apply(opts *handlerOptions) {
	f(opts)
}

func GzipLevel(level int) Option {
	return optionFunc(func(opts *handlerOptions) {
		if level < gzip.HuffmanOnly || level > gzip.BestCompression {
			panic(fmt.Errorf("httpenc: gzip: invalid compression level: %d", level))
		}
		opts.gzipLevel = level
	})
}

func DeflateLevel(level int) Option {
	return optionFunc(func(opts *handlerOptions) {
		if level < zlib.HuffmanOnly || level > zlib.BestCompression {
			panic(fmt.Errorf("httpenc: zlib: invalid compression level: %d", level))
		}
		opts.deflateLevel = level
	})
}

func BrotliLevel(level int) Option {
	return optionFunc(func(opts *handlerOptions) {
		if level < brotli.BestSpeed || level > brotli.BestCompression {
			panic(fmt.Errorf("httpenc: brotli: invalid compression level: %d", level))
		}
		opts.brotliLevel = level
	})
}
