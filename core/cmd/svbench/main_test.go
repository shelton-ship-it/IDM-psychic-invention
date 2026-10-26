package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// servidor que limita CADA ligação (~1 MiB/s), como muitos CDNs.
func throttledServer(t *testing.T, size int) *httptest.Server {
	data := make([]byte, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"b"`)
		http.ServeContent(&slow{ResponseWriter: w}, r, "f.bin", time.Unix(0, 0), bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type slow struct{ http.ResponseWriter }

func (w *slow) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := 16 << 10
		if c > len(p) {
			c = len(p)
		}
		m, err := w.ResponseWriter.Write(p[:c])
		n += m
		if err != nil {
			return n, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(15 * time.Millisecond)
		p = p[c:]
	}
	return n, nil
}

func TestBenchReportsSpeedupAgainstSingleConnection(t *testing.T) {
	srv := throttledServer(t, 2<<20)
	var out bytes.Buffer
	err := run(context.Background(), []string{"-url", srv.URL + "/f.bin?token=SEGREDO", "-connections", "1,4,8", "-min-segment-kb", "64"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	txt := out.String()
	if strings.Contains(txt, "SEGREDO") {
		t.Fatal("a query do URL não pode aparecer na saída")
	}
	re := regexp.MustCompile(`(?m)^8\s+[\d.]+\s+[\d.]+\s+×([\d.]+)`)
	m := re.FindStringSubmatch(txt)
	if m == nil {
		t.Fatalf("linha de 8 ligações em falta:\n%s", txt)
	}
	if x, _ := strconv.ParseFloat(m[1], 64); x < 3 {
		t.Fatalf("aceleração com 8 ligações devia ser ≥3×, foi ×%s\n%s", m[1], txt)
	}
	t.Log("\n" + txt)
}

func TestBenchValidationAndSizeLimit(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out); err == nil {
		t.Error("sem -url deve falhar")
	}
	for _, bad := range []string{"0", "33", "x", "1,,2"} {
		if err := run(context.Background(), []string{"-url", "http://x/", "-connections", bad}, &out); err == nil {
			t.Errorf("-connections %q deveria falhar", bad)
		}
	}
	srv := throttledServer(t, 3<<20)
	err := run(context.Background(), []string{"-url", srv.URL + "/f.bin", "-connections", "1", "-max-mb", "1"}, &out)
	if err == nil || !strings.Contains(err.Error(), "limite") {
		t.Errorf("ficheiro acima de -max-mb deve ser recusado com explicação: %v", err)
	}
}
