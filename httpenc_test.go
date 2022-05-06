package httpenc

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
)

var handlerTests = map[string]struct {
	path            string
	acceptEncoding  string
	contentEncoding string
	body            []byte
}{
	"precompression (gzip1)": {
		path:            "/test1.txt.gz",
		acceptEncoding:  "gzip,deflate,br",
		contentEncoding: "gzip",
		body:            []byte("Test 1"),
	},
	"precompression (gzip2)": {
		path:            "/test1.txt.gz",
		acceptEncoding:  "deflate,gzip,br",
		contentEncoding: "gzip",
		body:            []byte("Test 1"),
	},
	"precompression (brotli1)": {
		path:            "/test2.txt.br",
		acceptEncoding:  "br,gzip,deflate",
		contentEncoding: "br",
		body:            []byte("Test 2"),
	},
	"precompression (brotli2)": {
		path:            "/test2.txt.br",
		acceptEncoding:  "gzip,deflate,br",
		contentEncoding: "br",
		body:            []byte("Test 2"),
	},
	"decode precompression (gzip)": {
		path:            "/test1.txt.gz",
		acceptEncoding:  "",
		contentEncoding: "",
		body:            []byte("Test 1"),
	},
	"decode precompression (brotli)": {
		path:            "/test2.txt.br",
		acceptEncoding:  "",
		contentEncoding: "",
		body:            []byte("Test 2"),
	},
	"compression (gzip)": {
		path:            "/test3.txt",
		acceptEncoding:  "gzip,deflate,br",
		contentEncoding: "gzip",
		body:            []byte("Test 3"),
	},
	"compression (deflate)": {
		path:            "/test3.txt",
		acceptEncoding:  "deflate,gzip,br",
		contentEncoding: "deflate",
		body:            []byte("Test 3"),
	},
	"compression (brotli)": {
		path:            "/test3.txt",
		acceptEncoding:  "br,gzip,deflate",
		contentEncoding: "br",
		body:            []byte("Test 3"),
	},
	"no compression": {
		path:            "/test3.txt",
		acceptEncoding:  "",
		contentEncoding: "",
		body:            []byte("Test 3"),
	},
}

func TestHandler(t *testing.T) {
	server := httptest.NewServer(Handler(http.FileServer(http.Dir("./testdata"))))
	defer server.Close()
	serverURL := server.URL

	for name, tt := range handlerTests {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, serverURL+tt.path, nil)
			if err != nil {
				t.Fatalf("http.NewRequest(): error: %v", err)
				return
			}

			transport := &http.Transport{}
			if tt.acceptEncoding == "" {
				transport.DisableCompression = true
			} else {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			client := &http.Client{Transport: transport}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Get: error: %v", err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("invalid status: %s", resp.Status)
				return
			}

			bodyGot, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("io.ReadAll(resp.Body): error: %v", err)
				return
			}

			if tt.acceptEncoding != "" {
				enc := resp.Header.Get("Content-Encoding")
				if enc != tt.contentEncoding {
					t.Errorf("Content-Encoding is not match: got %#v, want %#v", enc, tt.contentEncoding)
				}

				var err error
				bodyGot, err = decodeBody(bodyGot, EncodingType(enc))
				if err != nil {
					t.Fatalf("decodeBody(): %v", err)
				}
			}

			if !bytes.Equal(bodyGot, tt.body) {
				t.Errorf("response body is not match: got %#v, want %#v", bodyGot, tt.body)
			}
		})
	}
}

func decodeBody(b []byte, enc EncodingType) ([]byte, error) {
	var r io.Reader
	switch enc {
	case Gzip:
		gr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		r = gr
	case Deflate:
		zr, err := zlib.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case Brotli:
		r = brotli.NewReader(bytes.NewReader(b))
	default:
		return nil, fmt.Errorf("unsupported encoding: %s", enc)
	}

	ret, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return ret, nil
}
