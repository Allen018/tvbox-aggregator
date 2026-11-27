// Package fetcher 实现并发抓取器：HTTP 连接池复用 + 宽容反序列化 + 脏数据隔离。
package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tvbox-aggregator/internal/model"
)

// defaultUserAgent 默认浏览器 UA，规避部分站点的 UA 校验。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// maxBodyBytes 响应体读取上限（32MB），防御异常响应耗尽内存。
const maxBodyBytes = 32 << 20

// Fetcher 带连接池的抓取器，供聚合主流程与测速器复用。
type Fetcher struct {
	client *http.Client
}

// New 创建抓取器。全局复用 http.Transport 连接池，
// 避免高频请求下的 TCP 握手开销与端口耗尽。
func New(timeout time.Duration) *Fetcher {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Fetcher{
		client: &http.Client{Transport: transport, Timeout: timeout},
	}
}

// Client 暴露内部 HTTP 客户端，供测速器复用同一连接池。
func (f *Fetcher) Client() *http.Client { return f.client }

// FetchRaw 拉取 URL 原始字节，返回响应体与 HTTP 状态码。
// 自动附带浏览器 UA，并按源配置合并自定义请求头。
func (f *Fetcher) FetchRaw(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "*/*")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	return body, resp.StatusCode, err
}

// FetchVodList 拉取采集站 API 并宽容解析视频列表。
// 遇到非标准 JSON 或空列表时返回错误，由上层记录为脏数据，不中断整体流程。
// 非 200 响应返回 *model.HTTPStatusError，供上层区分海外封锁/死链。
func (f *Fetcher) FetchVodList(ctx context.Context, url string, headers map[string]string) (*model.VodList, error) {
	body, status, err := f.FetchRaw(ctx, url, headers)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &model.HTTPStatusError{Status: status, URL: url}
	}
	var vl model.VodList
	if err := json.Unmarshal(body, &vl); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if len(vl.List) == 0 {
		return nil, fmt.Errorf("响应无视频列表(code=%d msg=%s)", vl.Code, vl.Msg)
	}
	return &vl, nil
}

// ExtractPlayURLs 从视频列表中提取前 max 个 m3u8 播放地址。
//
// 播放地址常见格式为 "第1集$https://x.m3u8#第2集$https://y.m3u8"，
// 多个播放来源以 $$$ 分隔；自动跳过预告片/片花等非正片，
// 并按出现顺序去重。
func ExtractPlayURLs(vl *model.VodList, max int) []string {
	if vl == nil || max <= 0 {
		return nil
	}
	urls := make([]string, 0, max)
	seen := map[string]bool{}
	add := func(u string) bool {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] || !strings.Contains(strings.ToLower(u), "m3u8") {
			return false
		}
		seen[u] = true
		urls = append(urls, u)
		return true
	}
	for _, item := range vl.List {
		for _, group := range strings.Split(item.PlayURL, "$$$") {
			for _, episode := range strings.Split(group, "#") {
				name, u := episode, episode
				if i := strings.Index(episode, "$"); i >= 0 {
					name, u = episode[:i], episode[i+1:]
				}
				if isTrailer(name) {
					continue
				}
				if add(u) && len(urls) >= max {
					return urls
				}
			}
		}
	}
	return urls
}

// isTrailer 判断剧集名是否为预告片/片花等非正片内容。
func isTrailer(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"预告", "片花", "花絮", "预告片", "trailer", "teaser"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}
