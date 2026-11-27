package tester

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"tvbox-aggregator/internal/model"
)

// rangeSize 单次探测读取的字节数（512KB）。
const rangeSize = 512 << 10

// defaultUserAgent 测速请求使用的浏览器 UA。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// HTTPStatusError 已统一由 model.HTTPStatusError 提供，供上层区分海外封锁/死链。

// ProbeResult 单次测速结果。
type ProbeResult struct {
	SpeedMBps float64 `json:"speed_mbps"`
	TTFBMs    int64   `json:"ttfb_ms"`
	TotalMs   int64   `json:"total_ms"`
	Bytes     int64   `json:"bytes"`
	RangeOK   bool    `json:"range_ok"`
}

// Prober Range 分片测速器。
type Prober struct {
	client  *http.Client
	timeout time.Duration
}

// NewProber 创建测速器，复用抓取器的连接池，单源超时强制熔断。
func NewProber(client *http.Client, timeout time.Duration) *Prober {
	return &Prober{client: client, timeout: timeout}
}

// Probe 对单个 URL 进行 Range 分片测速。
//
// 处理策略：
//   - 服务端支持 Range（206）→ 读取前 512KB；
//   - 返回 416（分片小于 512KB）→ 降级为全量读取该分片；
//   - 忽略 Range 返回 200 → 以 LimitReader 截断读取 512KB；
//   - 超时（默认 5 秒）强制熔断，单源故障绝不阻塞全局。
func (p *Prober) Probe(ctx context.Context, url string, headers map[string]string) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	res, status, err := p.probeOnce(ctx, url, headers, true)
	if err != nil {
		return res, err
	}
	if status == http.StatusRequestedRangeNotSatisfiable {
		// 416：分片小于探测范围，降级为全量读取
		res2, _, err2 := p.probeOnce(ctx, url, headers, false)
		if err2 != nil {
			return res2, err2
		}
		return res2, nil
	}
	if status >= 400 {
		return res, &model.HTTPStatusError{Status: status, URL: url}
	}
	return res, nil
}

// probeOnce 发起一次探测请求，useRange 控制是否携带 Range 头。
func (p *Prober) probeOnce(ctx context.Context, url string, headers map[string]string, useRange bool) (ProbeResult, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{}, 0, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "*/*")
	if useRange {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", rangeSize-1))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, 0, err
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)

	// 无论服务端返回 200（全量）还是 206（部分），都只读取前 512KB
	n, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, rangeSize))
	total := time.Since(start)

	res := ProbeResult{
		TTFBMs:  int64(ttfb.Milliseconds()),
		TotalMs: int64(total.Milliseconds()),
		Bytes:   n,
		RangeOK: resp.StatusCode == http.StatusPartialContent,
	}
	if n > 0 {
		elapsed := total.Seconds()
		if elapsed <= 0 {
			// 时钟精度兜底：连接复用且响应被完整缓冲时，往返耗时可能
			// 低于系统时钟粒度（测得恰好为 0），以 1ms 下限估算，避免速度被测为 0
			elapsed = 0.001
		}
		res.SpeedMBps = float64(n) / elapsed / (1 << 20)
	}
	if readErr != nil && n == 0 {
		return res, resp.StatusCode, readErr
	}
	return res, resp.StatusCode, nil
}

// AnalyzeResult 单源完整测速结果。
type AnalyzeResult struct {
	Playlist *Playlist
	Ad       AdReport
	Probe    ProbeResult
	Samples  int
}

// Analyze 完整测速流程：拉取 m3u8 → 主列表递归解析最高码率轨道 →
// 广告特征识别 → 跳过首分片、多分片样本投票测速。
//
// 返回的 error 为 *HTTPError 时表示服务端状态异常（如 403/503 封锁），
// 由上层分类处理。
func (p *Prober) Analyze(ctx context.Context, m3u8URL string, headers map[string]string) (*AnalyzeResult, error) {
	body, status, err := p.fetchBody(ctx, m3u8URL, headers)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &model.HTTPStatusError{Status: status, URL: m3u8URL}
	}

	var pl *Playlist
	if IsMaster(string(body)) {
		subs, err := ParseMaster(string(body), m3u8URL)
		if err != nil {
			return nil, err
		}
		best := ResolveBest(subs)
		subBody, subStatus, err := p.fetchBody(ctx, best.URL, headers)
		if err != nil {
			return nil, err
		}
		if subStatus != http.StatusOK {
			return nil, &model.HTTPStatusError{Status: subStatus, URL: best.URL}
		}
		pl, err = ParseMedia(string(subBody), best.URL)
		if err != nil {
			return nil, err
		}
	} else {
		pl, err = ParseMedia(string(body), m3u8URL)
		if err != nil {
			return nil, err
		}
	}

	ad := DetectAd(pl)

	// 样本投票：跳过首分片（可能含片头），取前 3 个有效分片各测一次，取最优。
	start := 0
	if len(pl.Segments) > 1 {
		start = 1
	}
	best := ProbeResult{}
	samples := 0
	for i := start; i < len(pl.Segments) && samples < 3; i++ {
		r, err := p.Probe(ctx, pl.Segments[i].URL, headers)
		if err != nil {
			continue
		}
		samples++
		if r.SpeedMBps > best.SpeedMBps {
			best = r
		}
	}
	if samples == 0 {
		return &AnalyzeResult{Playlist: pl, Ad: ad}, fmt.Errorf("所有样本测速失败")
	}
	return &AnalyzeResult{Playlist: pl, Ad: ad, Probe: best, Samples: samples}, nil
}

// fetchBody 拉取 m3u8 内容（上限 1MB）。
func (p *Prober) fetchBody(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "*/*")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}
