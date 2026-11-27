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
	StatusOK      = "ok"      // 测速成功
	StatusBlocked = "blocked" // 海外 IP 封锁（403/503）
	StatusFailed  = "failed"  // 死链 / 超时 / 无有效播放流
	StatusAd      = "ad"      // 含广告特征（降权保留）
	StatusStale   = "stale"   // 影片资源陈旧（latest vod_time 超阈值）
	StatusUnknown = "unknown" // 爬虫源免测 / 未参与测速
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

// idField 提取 ID 字段，兼容数字型 vod_id（如 12345）。
func idField(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var f FlexString
		if err := f.UnmarshalJSON(v); err == nil && strings.TrimSpace(string(f)) != "" {
			return strings.TrimSpace(string(f))
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

// stringSliceField 从原始 JSON 对象中按候选键依次提取字符串数组字段（宽容解析）。
func stringSliceField(raw map[string]json.RawMessage, keys ...string) []string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var items []string
		if err := json.Unmarshal(v, &items); err == nil && len(items) > 0 {
			return items
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// TVBox 客户端配置
// ---------------------------------------------------------------------------

// Site 是输出到 TVBox 客户端的单个站点配置，字段与主流客户端兼容。
type Site struct {
	// Key 是客户端必填主键，缺失会导致站点被整批忽略，必须输出。
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Type        *FlexInt `json:"type"` // 0=XML 采集 1=JSON 采集 3=爬虫 4=扩展
	API         string   `json:"api,omitempty"`
	Searchable  *FlexInt `json:"searchable,omitempty"`
	QuickSearch *FlexInt `json:"quickSearch,omitempty"`
	Filterable  *FlexInt `json:"filterable,omitempty"`
	Changeable  *FlexInt `json:"changeable,omitempty"`
	PlayerType  *FlexInt `json:"playerType,omitempty"`
	Ext         string   `json:"ext,omitempty"`
	Jar         string   `json:"jar,omitempty"` // 站点级 JAR，TVBox 实际读取的字段
	Spider      string   `json:"spider,omitempty"`
	Group       string   `json:"group,omitempty"` // 多仓版分组标注
	Categories  []string `json:"categories,omitempty"`

	// 聚合器内部字段，不输出到 JSON
	SourceURL string   `json:"-"`
	Tags      []string `json:"-"`
	Score     float64  `json:"-"`
	Status    string   `json:"-"`
	LatencyMs int64    `json:"-"` // 爬虫站延迟探测结果（TTFB），用于订阅内排序
	JarTTFB   int64    `json:"-"` // 依赖 JAR 的探测延迟（毫秒），用于优选全局 spider
	IsSpider  bool     `json:"-"` // 是否走爬虫站免测通道（不能仅用 type 判断，JSON CMS 也是 type 1）
}

// UnmarshalJSON 兼容 api/url/api_url、vod 名称异构等字段命名，逐字段宽容提取。
func (s *Site) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Key = stringField(raw, "key")
	s.Name = stringField(raw, "name", "site", "title", "key")
	s.Type = intField(raw, "type")
	s.API = stringField(raw, "api", "url", "api_url", "apiUrl")
	s.Searchable = intField(raw, "searchable")
	s.QuickSearch = intField(raw, "quickSearch", "quick_search")
	s.Filterable = intField(raw, "filterable", "filter")
	s.Changeable = intField(raw, "changeable")
	s.PlayerType = intField(raw, "playerType", "player_type")
	s.Ext = stringField(raw, "ext", "extend", "extends")
	s.Jar = stringField(raw, "jar")
	s.Spider = stringField(raw, "spider")
	s.Group = stringField(raw, "group", "category")
	s.Categories = stringSliceField(raw, "categories")
	return nil
}

// TVBoxConfig 是完整的 TVBox 订阅配置（单仓/多仓通用）。
// Spider 为全局爬虫 JAR；Lives/Parses 保持为空数组以兼容客户端对顶层字段的期待。
type TVBoxConfig struct {
	Spider string            `json:"spider,omitempty"`
	Sites  []Site            `json:"sites"`
	Lives  []json.RawMessage `json:"lives"`
	Parses []json.RawMessage `json:"parses"`
}

// UnmarshalJSON 解析订阅配置；单条站点解析失败时跳过该条，不中断整体（脏数据隔离）。
func (c *TVBoxConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Spider string            `json:"spider"`
		Sites  []json.RawMessage `json:"sites"`
		Lives  []json.RawMessage `json:"lives"`
		Parses []json.RawMessage `json:"parses"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Spider = raw.Spider
	c.Lives = raw.Lives
	c.Parses = raw.Parses
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
	ID       string // vod_id / id，用于 ac=detail 取播放地址
	Name     string
	PlayFrom string
	PlayURL  string
	Time     string // vod_time / vod_addtime，原始时间字符串
}

// UnmarshalJSON 兼容 vod_name/name/title、vod_play_url/play_url 等异构字段。
func (v *VodItem) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v.ID = idField(raw, "vod_id", "id", "vodId")
	v.Name = stringField(raw, "vod_name", "name", "title")
	v.PlayFrom = stringField(raw, "vod_play_from", "play_from", "from")
	v.PlayURL = stringField(raw, "vod_play_url", "play_url", "playurl", "url")
	v.Time = stringField(raw, "vod_time", "vod_addtime", "vod_date", "time")
	return nil
}

// ParseVodTime 解析苹果 CMS 的 vod_time：支持 Unix 秒/毫秒时间戳与常见日期格式。
// 解析失败返回零值时间。
func ParseVodTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(n, 0)
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05", "2006-01-02", "2006/01/02 15:04:05",
		"2006/01/02", "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
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
	FreshDays int       `json:"fresh_days"` // 最新影片距今天数；-1 表示未知
	Score     float64   `json:"score"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// SourceConfig 源池配置项（config/sources.yaml）。
type SourceConfig struct {
	Name       string            `yaml:"name"`
	Key        string            `yaml:"key"` // 站点唯一标识；订阅提取时保留上游原始 key
	URL        string            `yaml:"url"`
	API        string            `yaml:"api"`      // 站点 api 地址（采集站 API / 爬虫站 spider 名或 JS 地址）；缺省用 URL
	Type       string            `yaml:"type"`     // collect=采集站（可测速）spider=爬虫站（免测）subscription=订阅式源（抓取配置提取站点）
	Category   string            `yaml:"category"` // 分组标注（预留）
	Spider     string            `yaml:"spider"`   // 爬虫站 spider jar 原始地址（可被本地化重写）
	Jar        string            `yaml:"jar"`      // 站点级 JAR 地址（客户端读取字段）
	Ext        string            `yaml:"ext"`      // 爬虫扩展参数
	Categories []string          `yaml:"categories"`
	Headers    map[string]string `yaml:"headers"`     // 按源自定义请求头（反爬校验）
	Skip       bool              `yaml:"skip"`        // 跳过测速（仍参与配置输出）
	LocalJar   bool              `yaml:"local_jar"`   // 爬虫站 spider 地址是否重写为本地托管（需 jar 已放入仓库 jar/ 目录）
	OutputType int               `yaml:"output_type"` // 输出用类型数值（0/1/3），订阅提取时保留原始类型，客户端需要原始 type
	JarTTFB    int64             `yaml:"-"`           // 依赖 JAR 的探测延迟（毫秒），用于优选全局 spider
}

// WhitelistEntry 国内保底源白名单项（config/whitelist.yaml）。
// 命中白名单的采集站免测速，仅做存活探测并置顶标注 [国内直连]。
type WhitelistEntry struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Type    int               `yaml:"type"` // 输出类型：0=XML 采集，1=JSON 采集（默认 1）
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
	Sites     int       `json:"sites"` // 提取到的站点数
	URLs      int       `json:"urls"`  // 展开的子订阅地址数
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ScoreInput 多维评分输入。无法测速的 type 3 站点速度/新鲜度记为未知，
// 由调用方把 SpeedMBps/FreshKnown 置零，评分自然落后于实测源。
type ScoreInput struct {
	SpeedMBps float64
	TTFBMs    int64
	FreshDays int     // -1 表示未知
	Stability float64 // 0~1，本轮默认 1
	JarOK     bool    // 依赖 JAR 可用（对 type 3 有区分度）
	Ad        bool
}

// Score 归一化多维评分（0~100）：
// 速度 40% + 新鲜度 20% + TTFB 15% + 稳定性 10% + JAR 可用性 5% + 广告降权 5%。
// 未设置的维度按中性值 0.5 处理，避免未知信息被当成最差。
func Score(in ScoreInput) float64 {
	// 速度：log 归一化到 20MB/s，避免单个高速源碾压
	speed := 0.5
	if in.SpeedMBps > 0 {
		speed = math.Log1p(in.SpeedMBps) / math.Log1p(20)
		if speed > 1 {
			speed = 1
		}
	}
	// 新鲜度：≤7 天满分，30 天 0.7，60 天 0.3，>60 天 0；未知取 0.5
	fresh := 0.5
	if in.FreshDays >= 0 {
		switch d := in.FreshDays; {
		case d <= 7:
			fresh = 1
		case d <= 30:
			fresh = 1 - float64(d-7)/23*0.3
		case d <= 60:
			fresh = 0.7 - float64(d-30)/30*0.4
		default:
			fresh = 0
		}
	}
	// TTFB：5s 以上记 0
	ttfb := 0.5
	if in.TTFBMs >= 0 {
		ttfb = 1 - math.Min(float64(in.TTFBMs), 5000)/5000
	}
	stable := in.Stability
	if stable <= 0 || stable > 1 {
		stable = 0.5
	}
	jar := 0.5
	if in.JarOK {
		jar = 1
	}
	ad := 1.0
	if in.Ad {
		ad = 0
	}
	return 100 * (0.40*speed + 0.20*fresh + 0.15*ttfb + 0.10*stable + 0.05*jar + 0.05*ad)
}
