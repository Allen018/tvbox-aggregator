// Package model 定义 TVBox 聚合源的强类型数据结构与宽容解析逻辑。
//
// 第三方接口与 TVBox 客户端配置普遍存在字段类型漂移（type/searchable
// 时而为 int 时而为 string）与字段命名异构（api/url/key 等），本包通过
// 自定义 json.Unmarshaler 统一归一化，配合上层脏数据隔离，
// 避免上游脏数据导致运行时崩溃。
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 源测试状态常量。
const (
	StatusOK      = "ok"       // 测速成功
	StatusBlocked = "blocked"  // 海外 IP 封锁（403/503）
	StatusFailed  = "failed"   // 死链 / 超时 / 无有效播放流
	StatusAd      = "ad"       // 含广告特征（降权保留）
	StatusUnknown = "unknown"  // 爬虫源免测 / 未参与测速
)

// HTTPStatusError 携带 HTTP 状态码的类型化错误，供上层区分海外封锁/死链。
// 抓取与测速两个阶段统一使用该错误，保证分类口径一致。
type HTTPStatusError struct {
	Status int
	URL    string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.URL)
}

// ---------------------------------------------------------------------------
// 宽容基础类型
// ---------------------------------------------------------------------------

// FlexInt 兼容 JSON 中 int 与 string 混用的字段（如 type、searchable）。
type FlexInt struct {
	Value int
	Set   bool
}

// UnmarshalJSON 支持 null、数字、数字字符串、布尔值等形态，
// 无法解析时保持零值而不报错（脏数据容错）。
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	switch s {
	case "null", "":
		return nil
	case "true":
		f.Value, f.Set = 1, true
		return nil
	case "false":
		f.Value, f.Set = 0, true
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		f.Value, f.Set = n, true
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.TrimSpace(str)
		if str == "" {
			return nil
		}
		if n, err := strconv.Atoi(str); err == nil {
			f.Value, f.Set = n, true
			return nil
		}
		switch {
		case strings.EqualFold(str, "true"):
			f.Value, f.Set = 1, true
		case strings.EqualFold(str, "false"):
			f.Value, f.Set = 0, true
		}
	}
	return nil
}

// MarshalJSON 输出标准数字，未设置时输出 null。
func (f FlexInt) MarshalJSON() ([]byte, error) {
	if !f.Set {
		return []byte("null"), nil
	}
	return []byte(strconv.Itoa(f.Value)), nil
}

// FlexString 兼容 JSON 中 string 与数字混用的字段。
type FlexString string

// UnmarshalJSON 支持 null、字符串、数字等形态。
func (f *FlexString) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*f = FlexString(str)
		return nil
	}
	*f = FlexString(s)
	return nil
}

// IntPtr 构造已设置的 FlexInt 指针，用于可选字段输出（配合 omitempty）。
func IntPtr(v int) *FlexInt { return &FlexInt{Value: v, Set: true} }

// ---------------------------------------------------------------------------
// 宽容字段提取辅助
// ---------------------------------------------------------------------------

// stringField 从原始 JSON 对象中按候选键依次提取字符串字段。
func stringField(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var str string
		if err := json.Unmarshal(v, &str); err == nil && strings.TrimSpace(str) != "" {
			return strings.TrimSpace(str)
		}
	}
	return ""
}

// intField 从原始 JSON 对象中按候选键依次提取整数字段（宽容解析）。
func intField(raw map[string]json.RawMessage, keys ...string) *FlexInt {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var f FlexInt
		if err := f.UnmarshalJSON(v); err == nil && f.Set {
			return &f
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// TVBox 客户端配置
// ---------------------------------------------------------------------------

// Site 是输出到 TVBox 客户端的单个站点配置，字段与主流客户端兼容。
type Site struct {
	Name        string   `json:"name"`
	Type        *FlexInt `json:"type"` // 0=采集站 1=爬虫站
	API         string   `json:"api,omitempty"`
	Searchable  *FlexInt `json:"searchable,omitempty"`
	QuickSearch *FlexInt `json:"quickSearch,omitempty"`
	Filterable  *FlexInt `json:"filterable,omitempty"`
	PlayerType  *FlexInt `json:"playerType,omitempty"`
	Ext         string   `json:"ext,omitempty"`
	Jar         string   `json:"jar,omitempty"`
	Spider      string   `json:"spider,omitempty"`
	Group       string   `json:"group,omitempty"` // 多仓版分组标注

	// 聚合器内部字段，不输出到 JSON
	SourceURL string   `json:"-"`
	Tags      []string `json:"-"`
	Score     float64  `json:"-"`
	Status    string   `json:"-"`
	LatencyMs int64    `json:"-"` // 爬虫站延迟探测结果（TTFB），用于订阅内排序
}

// UnmarshalJSON 兼容 api/url/api_url、vod 名称异构等字段命名，逐字段宽容提取。
func (s *Site) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Name = stringField(raw, "name", "site", "title", "key")
	s.Type = intField(raw, "type")
	s.API = stringField(raw, "api", "url", "api_url", "apiUrl")
	s.Searchable = intField(raw, "searchable")
	s.QuickSearch = intField(raw, "quickSearch", "quick_search")
	s.Filterable = intField(raw, "filterable", "filter")
	s.PlayerType = intField(raw, "playerType", "player_type")
	s.Ext = stringField(raw, "ext", "extend", "extends")
	s.Jar = stringField(raw, "jar")
	s.Spider = stringField(raw, "spider")
	s.Group = stringField(raw, "group", "category")
	return nil
}

// TVBoxConfig 是完整的 TVBox 订阅配置（单仓/多仓通用）。
type TVBoxConfig struct {
	Sites []Site `json:"sites"`
}

// UnmarshalJSON 解析订阅配置；单条站点解析失败时跳过该条，不中断整体（脏数据隔离）。
func (c *TVBoxConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Sites []json.RawMessage `json:"sites"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Sites = make([]Site, 0, len(raw.Sites))
	for _, m := range raw.Sites {
		var s Site
		if err := json.Unmarshal(m, &s); err != nil {
			continue // 脏数据隔离：单条解析失败不中断整体
		}
		c.Sites = append(c.Sites, s)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 采集站 API 响应（苹果 CMS 兼容格式，宽容解析）
// ---------------------------------------------------------------------------

// VodItem 是采集站 API 返回的单个视频条目。
type VodItem struct {
	Name      string
	PlayFrom string
	PlayURL   string
}
// UnmarshalJSON 兼容 vod_name/name/title、vod_play_url/play_url 等异构字段。
func (v *VodItem) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v.Name = stringField(raw, "vod_name", "name", "title")
	v.PlayFrom = stringField(raw, "vod_play_from", "play_from", "from")
	v.PlayURL = stringField(raw, "vod_play_url", "play_url", "playurl", "url")
	return nil
}

// VodList 是采集站 API 的列表响应。
type VodList struct {
	Code int
	Msg  string
	List []VodItem
}

// UnmarshalJSON 兼容 list/data/result/vods 等列表容器字段，
// code 支持数字与字符串形态。
func (vl *VodList) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["code"]; ok {
		var f FlexInt
		if err := f.UnmarshalJSON(v); err == nil {
			vl.Code = f.Value
		}
	}
	vl.Msg = stringField(raw, "msg", "message", "info")
	for _, key := range []string{"list", "data", "result", "vods"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var items []VodItem
		if err := json.Unmarshal(v, &items); err == nil && len(items) > 0 {
			vl.List = items
			break
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 测速结果与配置模型
// ---------------------------------------------------------------------------

// TestResult 单源测速结果，同时用于 dist/status.json 诊断输出。
type TestResult struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Status    string    `json:"status"`
	SpeedMBps float64   `json:"speed_mbps"`
	TTFBMs    int64     `json:"ttfb_ms"`
	RangeOK   bool      `json:"range_ok"`
	Samples   int       `json:"samples"`
	AdTag     bool      `json:"ad_tag"`
	Score     float64   `json:"score"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// SourceConfig 源池配置项（config/sources.yaml）。
type SourceConfig struct {
	Name       string            `yaml:"name"`
	URL        string            `yaml:"url"`
	API        string            `yaml:"api"` // 站点 api 地址（采集站 API / 爬虫站 spider 名或 JS 地址）；缺省用 URL
	Type       string            `yaml:"type"` // collect=采集站（可测速）spider=爬虫站（免测）subscription=订阅式源（抓取配置提取站点）
	Category   string            `yaml:"category"` // 分组标注（预留）
	Spider     string            `yaml:"spider"`   // 爬虫站 spider jar 原始地址（可被本地化重写）
	Headers    map[string]string `yaml:"headers"`  // 按源自定义请求头（反爬校验）
	Skip       bool              `yaml:"skip"`     // 跳过测速（仍参与配置输出）
	LocalJar   bool              `yaml:"local_jar"` // 爬虫站 spider 地址是否重写为本地托管（需 jar 已放入仓库 jar/ 目录）
	OutputType int               `yaml:"-"`         // 输出用类型数值（0/1/3），订阅提取时保留原始类型，客户端需要原始 type
}

// WhitelistEntry 国内保底源白名单项（config/whitelist.yaml）。
// 命中白名单的采集站免测速，仅做存活探测并置顶标注 [国内直连]。
type WhitelistEntry struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Note    string            `yaml:"note"`
	Headers map[string]string `yaml:"headers"`
}

// WhitelistProbe 白名单源存活探测结果。
type WhitelistProbe struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Alive     bool      `json:"alive"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// SubscriptionProbe 订阅式源抓取诊断结果。
type SubscriptionProbe struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Status    string    `json:"status"` // ok=抓取成功 failed=失败
	TTFBMs    int64     `json:"ttfb_ms"`
	Sites     int       `json:"sites"`  // 提取到的站点数
	URLs      int       `json:"urls"`   // 展开的子订阅地址数
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Score 综合评分：实测带宽 × 0.7 + 响应速度 × 0.3。
// 响应速度将 TTFB 归一化到 0~10（100ms 记满分），与带宽量纲匹配。
func Score(speedMBps float64, ttfbMs int64) float64 {
	ttfbScore := 0.0
	if ttfbMs > 0 {
		ttfbScore = math.Min(1000.0/float64(ttfbMs), 10.0)
	}
	return speedMBps*0.7 + ttfbScore*0.3
}
