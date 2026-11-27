package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"tvbox-aggregator/internal/model"
)

// SubscriptionResult 订阅式源抓取结果。
type SubscriptionResult struct {
	Sites []model.Site // 解析出的站点（type 3 已映射为爬虫站）
	URLs  []string     // 配置中引用的其他订阅地址（一层展开用）
}

// FetchSubscription 抓取订阅式源配置，解析内部站点。
//
// 兼容两种格式：
//   - 站点配置：{"sites":[...]}（TVBox 订阅配置，可含全局 spider）
//   - 地址列表：{"urls":[{"url":"..."}]}（订阅地址索引）
//
// 相对 spider 路径（如 "./fty.jar;md5;xxx"）基于订阅 URL 解析为绝对地址，
// 全局 spider 应用于未单独声明 spider 的站点。
func (f *Fetcher) FetchSubscription(ctx context.Context, subURL string, headers map[string]string) (*SubscriptionResult, error) {
	body, status, err := f.FetchRaw(ctx, subURL, headers)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	body = transcodeBody(body)
	body = stripJSONComments(body)

	res := &SubscriptionResult{}

	// 解析全局 spider（订阅配置顶层字段，应用于全部站点）
	var global struct {
		Spider string `json:"spider"`
	}
	_ = json.Unmarshal(body, &global)

	// 优先解析站点配置
	var cfg model.TVBoxConfig
	if err := json.Unmarshal(body, &cfg); err == nil && len(cfg.Sites) > 0 {
		for _, s := range cfg.Sites {
			// TVBox 读取站点级 jar（不是 spider）；未声明 jar 的站点继承订阅全局 spider。
			if s.Jar == "" {
				s.Jar = global.Spider
			}
			s.Jar = resolveSpiderPath(subURL, s.Jar)
			res.Sites = append(res.Sites, normalizeSiteType(s))
		}
		return res, nil
	}

	// 退化为地址列表解析
	var doc struct {
		URLs []struct {
			URL string `json:"url"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		for _, u := range doc.URLs {
			if u.URL != "" {
				res.URLs = append(res.URLs, u.URL)
			}
		}
	}
	if len(res.URLs) == 0 {
		return nil, fmt.Errorf("无法识别的订阅配置格式（无 sites 且无 urls）")
	}
	return res, nil
}

// transcodeBody 将响应体转码为 UTF-8：
// 优先按 UTF-8 直接使用；非 UTF-8（如 GBK 编码的中文订阅配置，json.Unmarshal 会失败）
// 按 GBK 转码后再解析。
func transcodeBody(body []byte) []byte {
	if utf8.Valid(body) {
		return body
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(body)
	if err != nil {
		return body
	}
	return decoded
}

// stripJSONComments 去除 JSON 中的 // 行注释。
// 部分订阅配置会在顶部添加说明注释（// 开头），JSON 标准不允许注释，
// 需在解析前去除；字符串值内的 // 不受影响。
func stripJSONComments(body []byte) []byte {
	out := make([]byte, 0, len(body))
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(body) && body[i+1] == '/' {
			for i < len(body) && body[i] != '\n' && body[i] != '\r' {
				i++
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// normalizeSiteType 保留订阅配置中的原始类型数值（0/1/3），不做映射。
// 客户端需要原始 type（如 csp 源为 3），映射为 1 会导致客户端无法加载。
func normalizeSiteType(s model.Site) model.Site {
	return s
}

// resolveSpiderPath 将订阅配置中的相对 jar/spider 路径解析为绝对地址。
// 形如 "./fty.jar;md5;xxx" 的路径：分号前部分参与解析，保留 ";md5;xxx" 后缀供客户端校验。
func resolveSpiderPath(base, spider string) string {
	spider = strings.TrimSpace(spider)
	if spider == "" {
		return ""
	}
	path := spider
	suffix := ""
	if i := strings.Index(path, ";"); i >= 0 {
		path, suffix = path[:i], path[i:]
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.Contains(path, "://") {
		return path + suffix
	}
	u, err := url.Parse(base)
	if err != nil {
		return path + suffix
	}
	ref, err := u.Parse(path)
	if err != nil {
		return path + suffix
	}
	return ref.String() + suffix
}
