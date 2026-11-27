package tester

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tvbox-aggregator/internal/model"
)

// newSegmentPayload 构造指定大小的分片数据。
func newSegmentPayload(n int) []byte {
	return bytes.Repeat([]byte{1}, n)
}

func TestProbeRangeSupported(t *testing.T) {
	// 服务端支持 Range，返回 206
	payload := newSegmentPayload(1 << 20) // 1MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 0-524287/1048576")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[:rangeSize])
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	res, err := prober.Probe(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}
	if !res.RangeOK {
		t.Error("期望 RangeOK=true（服务端返回 206）")
	}
	if res.Bytes != rangeSize {
		t.Errorf("期望读取 %d 字节，实际 %d", rangeSize, res.Bytes)
	}
	if res.SpeedMBps <= 0 {
		t.Error("测速速度应 > 0")
	}
}

func TestProbeRangeNotSatisfiableFallback(t *testing.T) {
	// 分片仅 100KB，小于探测范围，服务端返回 416，应降级全量读取
	payload := newSegmentPayload(100 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	res, err := prober.Probe(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("416 降级后仍报错: %v", err)
	}
	if res.Bytes != int64(len(payload)) {
		t.Errorf("期望全量读取 %d 字节，实际 %d", len(payload), res.Bytes)
	}
	if res.RangeOK {
		t.Error("降级全量读取时 RangeOK 应为 false")
	}
	if res.SpeedMBps <= 0 {
		t.Error("降级后测速速度应 > 0")
	}
}

func TestProbeServerIgnoresRange(t *testing.T) {
	// 服务端忽略 Range，始终返回 200 全量，应截断读取 512KB
	payload := newSegmentPayload(1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	res, err := prober.Probe(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}
	if res.RangeOK {
		t.Error("服务端忽略 Range 时 RangeOK 应为 false")
	}
	if res.Bytes != rangeSize {
		t.Errorf("期望截断读取 %d 字节，实际 %d", rangeSize, res.Bytes)
	}
}

func TestProbeForbidden(t *testing.T) {
	// 403 应返回 *HTTPError 供上层分类为海外封锁
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	_, err := prober.Probe(context.Background(), srv.URL, nil)
	var he *model.HTTPStatusError
	if !errors.As(err, &he) {
		t.Fatalf("期望 *model.HTTPStatusError，实际: %v", err)
	}
	if he.Status != http.StatusForbidden {
		t.Errorf("期望状态 403，实际 %d", he.Status)
	}
}

const mediaPlaylistTemplate = `#EXTM3U
#EXT-X-VERSION:3
#EXTINF:10.0,
seg0.ts
#EXTINF:10.0,
seg1.ts
#EXTINF:10.0,
seg2.ts
#EXTINF:10.0,
seg3.ts
#EXT-X-ENDLIST
`

func TestAnalyzeMediaPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, mediaPlaylistTemplate)
			return
		}
		// 分片数据
		w.Write(newSegmentPayload(1 << 20))
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	ana, err := prober.Analyze(context.Background(), srv.URL+"/index.m3u8", nil)
	if err != nil {
		t.Fatalf("Analyze 报错: %v", err)
	}
	if ana.Samples != 3 {
		t.Errorf("期望 3 个测速样本（跳过首分片），实际 %d", ana.Samples)
	}
	if ana.Probe.SpeedMBps <= 0 {
		t.Error("测速速度应 > 0")
	}
	if ana.Playlist.Discontinuities != 0 || ana.Ad.Suspicious() {
		t.Errorf("干净播放列表不应判定含广告: %+v", ana.Ad)
	}
}

func TestAnalyzeMasterPlaylist(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=1280x720\n720p/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\n1080p/index.m3u8\n"

	var requestedSub string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			io.WriteString(w, master)
		case "/720p/index.m3u8", "/1080p/index.m3u8":
			if strings.Contains(r.URL.Path, "1080p") {
				requestedSub = r.URL.Path
			}
			io.WriteString(w, mediaPlaylistTemplate)
		default:
			w.Write(newSegmentPayload(1 << 20))
		}
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	ana, err := prober.Analyze(context.Background(), srv.URL+"/master.m3u8", nil)
	if err != nil {
		t.Fatalf("Analyze 报错: %v", err)
	}
	if requestedSub != "/1080p/index.m3u8" {
		t.Errorf("应递归选择最高码率 1080p 轨道，实际请求 %q", requestedSub)
	}
	if ana.Samples != 3 || ana.Probe.SpeedMBps <= 0 {
		t.Errorf("测速结果异常: samples=%d speed=%.2f", ana.Samples, ana.Probe.SpeedMBps)
	}
}

func TestAnalyzeBlocked(t *testing.T) {
	// m3u8 返回 403，应返回 *HTTPError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 5*time.Second)
	_, err := prober.Analyze(context.Background(), srv.URL+"/index.m3u8", nil)
	var he *model.HTTPStatusError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden {
		t.Fatalf("期望 403 的 *model.HTTPStatusError，实际: %v", err)
	}
}

func TestProbeTimeoutCircuitBreak(t *testing.T) {
	// 服务端不响应，5 秒超时应熔断而非挂死
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer srv.Close()

	prober := NewProber(srv.Client(), 500*time.Millisecond)
	start := time.Now()
	_, err := prober.Probe(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时熔断耗时过长: %v", elapsed)
	}
}

func TestFormatRangeHeader(t *testing.T) {
	// 仅验证格式串（避免引入额外请求）
	got := fmt.Sprintf("bytes=0-%d", rangeSize-1)
	if got != "bytes=0-524287" {
		t.Errorf("Range 头格式错误: %q", got)
	}
}
