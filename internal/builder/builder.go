// Package builder 实现结果清洗、智能打标、JAR 本地化重写与多版本产物输出。
package builder

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tvbox-aggregator/internal/model"
)

// defaultTopN 订阅源默认收录数量。
const defaultTopN = 50

// excludeCategoryKeywords 订阅清洗：排除的内容类别关键词。
// 覆盖教育、网盘、磁力、相声、小品、音乐、戏曲、体育、央视/直播、工具/存储等非影视点播内容。
// 通用影视源（名称不含排除关键词）予以保留。
var excludeCategoryKeywords = []string{
	// 教育
	"教育", "课堂", "小学", "初中", "高中", "少儿", "启蒙", "教学",
	// 网盘
	"网盘", "云盘", "夸克", "夸父", "百度云", "阿里云盘", "alist", "alips", "分享", "聚盘", "盘他",
	// 磁力
	"磁力", "6v",
	// 相声
	"相声",
	// 小品
	"小品",
	// 音乐
	"音乐", "mv", "ktv", "音悦台", "酷狗", "歌词", "有声",
	// 戏曲
	"戏曲",
	// 体育
	"体育", "看球", "足球", "篮球", "赛事",
	// 央视/直播
	"央视", "直播",
	// 工具/存储
	"本地存储", "云搜", "小纸条", "Up搜",
}

// isExcludedCategory 判断站点名称是否属于排除的内容类别（大小写不敏感）。
func isExcludedCategory(name string) bool {
	lower := strings.ToLower(name)
	for _, k := range excludeCategoryKeywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// Builder 产物构建器。
type Builder struct {
	JarBaseURL string // 本地托管 jar 的基础地址（如 https://xxx.pages.dev/jar），为空则不重写
	OutDir     string // 产物输出目录
}

// New 创建产物构建器。
func New(jarBaseURL, outDir string) *Builder {
	return &Builder{JarBaseURL: jarBaseURL, OutDir: outDir}
}

// Build 生成产物：
//   - tvbox.json   单一订阅版（清洗后按规则排序取前 topN，默认 15）
//   - status.json  全量诊断信息
//
// discovered 为自动发现通道产出的源 URL 集合，其站点在产物中标注 [新发现]。
// subProbes 为订阅式源抓取诊断结果，仅用于 status.json。
func (b *Builder) Build(
	sources []model.SourceConfig,
	results map[string]*model.TestResult,
	whitelist []model.WhitelistEntry,
	probes map[string]*model.WhitelistProbe,
	discovered map[string]bool,
	topN int,
	subProbes map[string]*model.SubscriptionProbe,
) error {
	if err := os.MkdirAll(b.OutDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 白名单 URL 集合，用于标记与置顶
	wlURLs := map[string]model.WhitelistEntry{}
	for _, e := range whitelist {
		wlURLs[e.URL] = e
	}

	// 组装站点条目
	var sites []model.Site
	for _, src := range sources {
		sites = append(sites, b.toSite(src, results[src.URL], wlURLs[src.URL].Name != "", discovered[src.URL]))
	}
	// 白名单中不在源池的条目也输出
	existing := map[string]bool{}
	for _, s := range sites {
		existing[s.SourceURL] = true
	}
	for _, e := range whitelist {
		if existing[e.URL] {
			continue
		}
		sites = append(sites, model.Site{
			Name:       joinTags(e.Name, []string{"[国内直连]"}),
			Type:       model.IntPtr(0),
			API:        e.URL,
			Searchable: model.IntPtr(1),
			SourceURL:  e.URL,
			Status:     model.StatusOK,
			Tags:       []string{"[国内直连]"},
		})
	}

	if err := b.writeJSON("tvbox.json", b.buildSubscription(sites, probes, topN)); err != nil {
		return err
	}
	return b.writeJSON("status.json", b.buildStatus(sources, results, whitelist, probes, subProbes))
}

// toSite 由源配置与测速结果组装输出站点条目，并将标签折叠进名称。
func (b *Builder) toSite(src model.SourceConfig, res *model.TestResult, whitelisted, discovered bool) model.Site {
	api := src.API
	if api == "" {
		api = src.URL
	}
	s := model.Site{
		Name:        src.Name,
		Type:        model.IntPtr(0),
		API:         api,
		Searchable:  model.IntPtr(1),
		QuickSearch: model.IntPtr(1),
		SourceURL:   src.URL,
	}
	if src.Type == "spider" {
		s.Type = model.IntPtr(1)
		s.Spider = src.Spider
		if res != nil {
			// 爬虫站延迟探测结果（TTFB），用于订阅内排序
			s.LatencyMs = res.TTFBMs
		}
	}
	if src.Category != "" {
		s.Group = src.Category
	}

	var tags []string
	if whitelisted {
		// 白名单源免测速：置顶标注 [国内直连]
		tags = append(tags, "[国内直连]")
		s.Status = model.StatusOK
	} else if res != nil {
		s.Status = res.Status
		s.Score = res.Score
		if src.Type != "spider" {
			// 爬虫站无测速数据，不输出速度类标签
			switch {
			case res.Status == model.StatusOK:
				tags = append(tags, fmt.Sprintf("🚀 %.1fMB/s", res.SpeedMBps))
			case res.Status == model.StatusAd:
				tags = append(tags, fmt.Sprintf("⚠️含广告 %.1fMB/s", res.SpeedMBps))
			case res.Status == model.StatusBlocked:
				tags = append(tags, "🔒海外封锁")
			case res.Status == model.StatusFailed:
				tags = append(tags, "❌失效")
			}
		}
	} else {
		s.Status = model.StatusUnknown
	}
	if discovered {
		// 自动发现源：标注来源，与人工维护源区分
		tags = append(tags, "[新发现]")
	}
	s.Tags = tags
	s.Name = joinTags(src.Name, tags)

	// JAR 本地化重写（仅配置了 local_jar 的爬虫站；订阅提取的第三方 jar 保持原地址）
	if src.Type == "spider" && src.LocalJar {
		s = b.rewriteJar(s)
	}
	return s
}

// joinTags 将标签折叠进站点名称（客户端仅展示 name）。
func joinTags(name string, tags []string) string {
	if len(tags) == 0 {
		return name
	}
	return name + " " + strings.Join(tags, " ")
}

// rewriteJar 将外部 spider/jar 地址重写为本地托管地址，防上游失效与篡改。
func (b *Builder) rewriteJar(s model.Site) model.Site {
	if b.JarBaseURL == "" {
		return s
	}
	if s.Spider != "" {
		s.Spider = b.localize(s.Spider)
	}
	if s.Jar != "" {
		s.Jar = b.localize(s.Jar)
	}
	return s
}

// localize 保留原文件名，替换为本地托管路径。
func (b *Builder) localize(original string) string {
	name := path.Base(original)
	if name == "." || name == "/" || name == "" || name == "jar" {
		name = "spider.jar"
	}
	return strings.TrimRight(b.JarBaseURL, "/") + "/" + name
}

// hasTag 判断站点是否携带指定标签。
func hasTag(s model.Site, tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// spiderLatency 返回爬虫站排序用延迟：仅探测成功（OK）者使用实测 TTFB，
// 失败/封锁/未探测者视为最大，排最后（失败探测的耗时不是真实延迟，不参与排序）。
func spiderLatency(s model.Site) int64 {
	if s.LatencyMs <= 0 || s.Status != model.StatusOK {
		return math.MaxInt64
	}
	return s.LatencyMs
}

// sortSpiderDiverse 爬虫站排序：已探测（OK）的按订阅组轮询交错，
// 保证订阅来源多样性（不全部来自同一订阅）；组按延迟升序，组内按名称排序；
// 未探测/失败的站点排在最后。
func sortSpiderDiverse(spider []model.Site) []model.Site {
	var measured, unmeasured []model.Site
	for _, s := range spider {
		if s.Status == model.StatusOK {
			measured = append(measured, s)
		} else {
			unmeasured = append(unmeasured, s)
		}
	}

	// 按订阅组分组（SourceURL 为组键）
	groups := map[string][]model.Site{}
	var order []string
	for _, s := range measured {
		g := s.SourceURL
		if groups[g] == nil {
			order = append(order, g)
		}
		groups[g] = append(groups[g], s)
	}
	// 组内按名称排序（稳定）
	for _, g := range order {
		sort.SliceStable(groups[g], func(i, j int) bool { return groups[g][i].Name < groups[g][j].Name })
	}
	// 组按延迟升序（与原项目 _lat 排序对齐）
	sort.SliceStable(order, func(i, j int) bool {
		return spiderLatency(groups[order[i]][0]) < spiderLatency(groups[order[j]][0])
	})

	// 轮询交错：每轮从各组各取一个
	out := make([]model.Site, 0, len(spider))
	maxLen := 0
	for _, g := range order {
		if len(groups[g]) > maxLen {
			maxLen = len(groups[g])
		}
	}
	for i := 0; i < maxLen; i++ {
		for _, g := range order {
			if i < len(groups[g]) {
				out = append(out, groups[g][i])
			}
		}
	}
	// 未探测/失败的排在最后
	out = append(out, unmeasured...)
	return out
}

// buildSubscription 单一订阅版：清洗后排序取前 topN。
//
// 清洗规则：
//   - 采集站仅保留测速成功且无广告者（失效/封锁/含广告剔除）；
//   - 白名单源仅保留存活探测通过者；
//   - 爬虫站免测保留（无实测速度，不参与速度排序）。
//
// 排序与名额：白名单源（国内直连）置顶，爬虫站保底，
// 测速成功源按综合评分（实测带宽 × 0.7 + 响应速度 × 0.3）降序填充剩余名额，
// 总数量截取前 topN。
func (b *Builder) buildSubscription(sites []model.Site, probes map[string]*model.WhitelistProbe, topN int) *model.TVBoxConfig {
	if topN <= 0 {
		topN = defaultTopN
	}
	var wl, ranked, spider []model.Site
	for _, s := range sites {
		// 内容类别排除（教育/网盘/磁力/相声/小品/音乐/戏曲/体育/央视直播/工具存储）
		if isExcludedCategory(s.Name) {
			continue
		}
		if s.Type != nil && s.Type.Value == 1 {
			spider = append(spider, s)
			continue
		}
		if hasTag(s, "[国内直连]") {
			// 白名单源：仅存活探测通过者保留
			if p, ok := probes[s.SourceURL]; !ok || p == nil || p.Alive {
				wl = append(wl, s)
			}
			continue
		}
		// 清洗：仅保留测速成功且无广告的采集站
		if s.Status == model.StatusOK && !hasTag(s, "⚠️含广告") {
			ranked = append(ranked, s)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	// 爬虫站排序：按订阅组轮询交错，保证来源多样性（不全部来自同一订阅）；
	// 未探测/失败的排在最后
	spider = sortSpiderDiverse(spider)

	// 名额分配：白名单与爬虫站优先保留，速度排序源填充剩余名额
	room := topN - len(wl) - len(spider)
	if room < 0 {
		room = 0
	}
	if len(ranked) > room {
		ranked = ranked[:room]
	}
	kept := make([]model.Site, 0, len(wl)+len(ranked)+len(spider))
	kept = append(kept, wl...)
	kept = append(kept, ranked...)
	kept = append(kept, spider...)
	if len(kept) > topN {
		kept = kept[:topN] // 极端情况（白名单+爬虫站超限）兜底截取
	}
	return &model.TVBoxConfig{Sites: kept}
}

// ---------------------------------------------------------------------------
// 诊断产物
// ---------------------------------------------------------------------------

// StatusSummary 产物诊断摘要。
type StatusSummary struct {
	GeneratedAt time.Time `json:"generated_at"`
	Total       int       `json:"total"`
	OK          int       `json:"ok"`
	Blocked     int       `json:"blocked"`
	Failed      int       `json:"failed"`
	Ad          int       `json:"ad"`
	Spider      int       `json:"spider"`
	Whitelist   int       `json:"whitelist"`
}

// StatusReport 完整诊断报告（dist/status.json）。
type StatusReport struct {
	Summary       StatusSummary             `json:"summary"`
	Sources       []*model.TestResult       `json:"sources"`
	Whitelist     []model.WhitelistProbe    `json:"whitelist"`
	Subscriptions []model.SubscriptionProbe `json:"subscriptions"`
}

// buildStatus 汇总全部测速、白名单探测与订阅抓取诊断结果。
func (b *Builder) buildStatus(
	sources []model.SourceConfig,
	results map[string]*model.TestResult,
	whitelist []model.WhitelistEntry,
	probes map[string]*model.WhitelistProbe,
	subProbes map[string]*model.SubscriptionProbe,
) *StatusReport {
	rep := &StatusReport{
		Summary:       StatusSummary{GeneratedAt: time.Now().UTC()},
		Sources:       []*model.TestResult{},
		Whitelist:     []model.WhitelistProbe{},
		Subscriptions: []model.SubscriptionProbe{},
	}
	for _, src := range sources {
		rep.Summary.Total++
		res := results[src.URL]
		if res == nil {
			continue
		}
		// 爬虫站的延迟探测结果同样纳入诊断（含状态与 TTFB）
		rep.Sources = append(rep.Sources, res)
		if src.Type == "spider" {
			rep.Summary.Spider++
			continue
		}
		switch res.Status {
		case model.StatusOK:
			rep.Summary.OK++
		case model.StatusBlocked:
			rep.Summary.Blocked++
		case model.StatusAd:
			rep.Summary.Ad++
		case model.StatusFailed:
			rep.Summary.Failed++
		}
	}
	for _, e := range whitelist {
		rep.Summary.Whitelist++
		if p, ok := probes[e.URL]; ok && p != nil {
			rep.Whitelist = append(rep.Whitelist, *p)
		} else {
			rep.Whitelist = append(rep.Whitelist, model.WhitelistProbe{
				Name: e.Name, URL: e.URL, CheckedAt: time.Now().UTC(), Error: "未探测",
			})
		}
	}
	for _, p := range subProbes {
		rep.Subscriptions = append(rep.Subscriptions, *p)
	}
	return rep
}

// writeJSON 以缩进格式写出 JSON 产物。
func (b *Builder) writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", name, err)
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(b.OutDir, name), data, 0o644)
}
