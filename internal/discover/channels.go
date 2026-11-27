package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// urlRegex 提取文本中的 http(s) 链接（兼容 HTML 与 JSON 文本）。
var urlRegex = regexp.MustCompile(`https?://[^\s"'<>\)\]\}，。、；：,]+`)

// extractURLs 从页面/片段文本中提取候选链接，去除尾部标点并去重。
func extractURLs(text string) []string {
	matches := urlRegex.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		m = strings.TrimRight(m, ".,;:!?)\"'")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// candidateURLs 生成候选的探测地址：
// 若候选本身已是采集 API 路径（含 provide/vod）则直接探测，
// 否则尝试该域名的标准端点组合（采集站多为 http，优先 http）。
func candidateURLs(candidate string) []string {
	candidate = strings.TrimSpace(candidate)
	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" {
		return nil
	}
	if strings.Contains(u.Path, "provide/vod") {
		return []string{candidate}
	}
	schemes := []string{"http://", "https://"}
	paths := []string{"/motif/api.php/provide/vod", "/api.php/provide/vod"}
	out := make([]string, 0, len(schemes)*len(paths))
	for _, s := range schemes {
		for _, p := range paths {
			out = append(out, s+u.Host+p)
		}
	}
	return out
}

// siteName 由域名生成站点名。
func siteName(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	return "发现·" + parsed.Host
}

// githubChannel 通过 GitHub 代码搜索 API 发现包含采集 API 地址的公开配置。
// 仅使用官方 Search API（不抓取搜索结果页），要求 Token 认证。
func (d *Discoverer) githubChannel(ctx context.Context) ([]string, error) {
	if d.token == "" {
		return nil, fmt.Errorf("未配置 GitHub Token（env: GITHUB_TOKEN），跳过代码搜索通道")
	}
	base := d.cfg.GitHub.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	perPage := d.cfg.GitHub.PerPage
	if perPage <= 0 {
		perPage = 100
	}

	var all []string
	for _, q := range d.cfg.GitHub.Queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		apiURL := fmt.Sprintf("%s/search/code?q=%s&per_page=%d",
			base, url.QueryEscape(q), perPage)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+d.token)
		req.Header.Set("Accept", "application/vnd.github.text-match+json")
		req.Header.Set("User-Agent", "tvbox-aggregator")

		resp, err := d.fetcher.Client().Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub API HTTP %d: %.200s", resp.StatusCode, string(body))
		}

		var result struct {
			Items []struct {
				TextMatches []struct {
					Fragment string `json:"fragment"`
				} `json:"text_matches"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, err
		}
		for _, item := range result.Items {
			for _, tm := range item.TextMatches {
				all = append(all, extractURLs(tm.Fragment)...)
			}
		}
	}
	return all, nil
}

// indexChannel 抓取社区聚合索引站页面，提取其中的采集站链接。
// 页面格式不做假设：统一按文本提取 http(s) 链接，交由验证阶段过滤。
func (d *Discoverer) indexChannel(ctx context.Context, pageURL string) ([]string, error) {
	body, status, err := d.fetcher.FetchRaw(ctx, pageURL, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	return extractURLs(string(body)), nil
}
