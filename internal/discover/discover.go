// Package discover 实现采集站自动发现：从 GitHub 代码搜索与社区聚合索引站
// 两个通道收集候选源，验证标准 API 端点后产出可用采集站列表，
// 供主流程合并入池、测速与输出。
//
// 合规边界：仅探测公开 HTTP 端点（标准采集 API 路径），
// 不突破反爬、不绕过 robots、不访问需认证内容；全局限速、低频率运行。
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"tvbox-aggregator/internal/fetcher"
	"tvbox-aggregator/internal/model"
)

// validateTimeout 验证阶段的总时限，防止大量死链拖垮单轮运行。
const validateTimeout = 90 * time.Second

// validateConcurrency 候选验证并发度。
const validateConcurrency = 20

// Config 发现模块配置（config/discover.yaml）。
type Config struct {
	Enabled    bool         `yaml:"enabled"`
	GitHub     GitHubConfig `yaml:"github"`
	Clbug      ClbugConfig  `yaml:"clbug"`
	IndexSites []IndexSite  `yaml:"index_sites"`
	MaxSources int          `yaml:"max_sources"` // 每轮最多收录数量，控制测速耗时
}

// GitHubConfig GitHub 代码搜索通道配置。
type GitHubConfig struct {
	Enabled bool     `yaml:"enabled"`
	Queries []string `yaml:"queries"`  // 搜索关键词，逐条执行
	PerPage int      `yaml:"per_page"` // 单次搜索返回条数
	Pages   int      `yaml:"pages"`    // 每个关键词的搜索页数，默认 1
	BaseURL string   `yaml:"base_url"` // 默认 https://api.github.com，测试可替换
}

// IndexSite 社区聚合索引站配置。
type IndexSite struct {
	URL           string `yaml:"url"`
	MaxCandidates int    `yaml:"max_candidates"` // 单站最多提取 URL 数，默认 80
}

// ClbugConfig 社区聚合页发现通道（tvbox.clbug.com/user.php）。
// 该页只提供“订阅地址列表”，真正的配置来自页面中的每个第三方 URL。
type ClbugConfig struct {
	Enabled       bool   `yaml:"enabled"`
	URL           string `yaml:"url"`
	MaxCandidates int    `yaml:"max_candidates"`
}

// clbugURLRegex 提取页面中 data-url="..." 属性。
var clbugURLRegex = regexp.MustCompile(`data-url="([^"]+)"`)

// Discoverer 采集站发现器。
type Discoverer struct {
	cfg     Config
	fetcher *fetcher.Fetcher
	token   string // GitHub API Token，为空时跳过代码搜索通道
}

// New 创建发现器。
func New(cfg Config, f *fetcher.Fetcher, token string) *Discoverer {
	if cfg.MaxSources <= 0 {
		cfg.MaxSources = 60
	}
	return &Discoverer{cfg: cfg, fetcher: f, token: token}
}

// Run 执行发现全流程：多通道收集候选 → 去重 → 合并上轮观察池 → 验证 →
// 按观察期规则返回“本轮可正式入池”的源，同时返回完整观察池供落盘。
//
// 观察期规则：连续 2 轮验证成功才晋升（Promoted）；失败保留 3 轮宽限后淘汰。
func (d *Discoverer) Run(ctx context.Context, existing map[string]bool, prev []model.SourceConfig) ([]model.SourceConfig, []model.SourceConfig, error) {
	var candidates []string

	// 通道一：GitHub 代码搜索（官方 Search API，支持分页）
	if d.cfg.GitHub.Enabled {
		found, err := d.githubChannel(ctx)
		if err != nil {
			log.Printf("GitHub 代码搜索通道失败（跳过）: %v", err)
		}
		candidates = append(candidates, found...)
	}

	// 通道二：社区聚合页 tvbox.clbug.com/user.php（提取 data-url 订阅地址）
	if d.cfg.Clbug.Enabled {
		found, err := d.clbugChannel(ctx)
		if err != nil {
			log.Printf("clbug 发现通道失败（跳过）: %v", err)
		}
		candidates = append(candidates, found...)
	}

	// 通道三：社区聚合索引站（通用链接提取，兜底）
	for _, site := range d.cfg.IndexSites {
		found, err := d.indexChannel(ctx, site.URL)
		if err != nil {
			log.Printf("索引站 %s 抓取失败（跳过）: %v", site.URL, err)
			continue
		}
		limit := site.MaxCandidates
		if limit <= 0 {
			limit = 80
		}
		if len(found) > limit {
			found = found[:limit]
		}
		candidates = append(candidates, found...)
	}

	// 上轮观察池（含未晋升候选）作为跟踪基础
	tracked := make(map[string]*model.SourceConfig, len(prev)+len(candidates))
	for i := range prev {
		p := prev[i]
		if p.URL != "" {
			entry := p
			tracked[p.URL] = &entry
		}
	}

	// 新候选去重 + 排除已有源 + 合入跟踪池
	seen := map[string]bool{}
	unique := make([]string, 0, len(candidates)+len(tracked))
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] || existing[c] {
			continue
		}
		seen[c] = true
		unique = append(unique, c)
	}
	for url := range tracked {
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		unique = append(unique, url)
	}
	sort.Strings(unique)
	log.Printf("发现候选源 %d 个（含上轮观察池 %d），开始验证", len(unique), len(tracked))

	// 并发验证（受总时限约束）
	valCtx, valCancel := context.WithTimeout(ctx, validateTimeout)
	defer valCancel()
	results := d.validateAll(valCtx, unique)

	promoted := make([]model.SourceConfig, 0, len(unique))
	pool := make([]model.SourceConfig, 0, len(unique))
	for _, url := range unique {
		entry := tracked[url]
		if entry == nil {
			entry = &model.SourceConfig{URL: url}
		}
		if src, ok := results[url]; ok && src != nil {
			entry.Name, entry.Type = src.Name, src.Type
			entry.SuccessRuns++
			entry.FailRuns = 0
		} else {
			entry.FailRuns++
		}
		if entry.FailRuns > 3 {
			continue // 连续失败 4 轮，淘汰
		}
		entry.Promoted = entry.SuccessRuns >= 2
		pool = append(pool, *entry)
		if entry.Promoted && len(promoted) < d.cfg.MaxSources {
			promoted = append(promoted, *entry)
		}
	}
	// 观察池上限，避免无限增长
	if maxPool := d.cfg.MaxSources * 3; len(pool) > maxPool {
		pool = pool[:maxPool]
	}
	log.Printf("发现完成: 验证通过 %d / 观察池 %d / 本轮晋升 %d", len(results), len(pool), len(promoted))
	return promoted, pool, nil
}

// validateAll 并发验证候选源，返回 URL → 验证通过的源映射（未出现的即失败）。
func (d *Discoverer) validateAll(ctx context.Context, candidates []string) map[string]*model.SourceConfig {
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, validateConcurrency)
	out := make(map[string]*model.SourceConfig, len(candidates))

	for _, c := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(c string) {
			defer wg.Done()
			defer func() { <-sem }()
			if src := d.validate(ctx, c); src != nil {
				mu.Lock()
				out[c] = src
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return out
}

// validate 验证候选：
//   - 若候选本身是 TVBox 订阅配置（sites / urls），作为 subscription 收录；
//   - 否则按采集站 API 探测标准端点（HTTP 200 + 合法 JSON + 有视频列表）。
func (d *Discoverer) validate(ctx context.Context, candidate string) *model.SourceConfig {
	// 订阅配置优先：clbug 页面给出的都是订阅地址，不能按采集 API 探测。
	if res, err := d.fetcher.FetchSubscription(ctx, candidate, nil); err == nil &&
		(len(res.Sites) > 0 || len(res.URLs) > 0) {
		return &model.SourceConfig{
			Name: siteName(candidate),
			URL:  candidate,
			Type: "subscription",
		}
	}
	for _, u := range candidateURLs(candidate) {
		body, status, err := d.fetcher.FetchRaw(ctx, u, nil)
		if err != nil || status != 200 {
			continue
		}
		var vl model.VodList
		if err := json.Unmarshal(body, &vl); err != nil || len(vl.List) == 0 {
			continue
		}
		return &model.SourceConfig{
			Name: siteName(u),
			URL:  u,
			Type: "collect",
		}
	}
	return nil
}

// clbugChannel 抓取社区聚合页并提取 data-url 订阅地址。
// 这里只发现“订阅地址”，不做信任：后续统一经 validate 验证配置可解析后才入池。
func (d *Discoverer) clbugChannel(ctx context.Context) ([]string, error) {
	u := d.cfg.Clbug.URL
	if u == "" {
		u = "https://tvbox.clbug.com/user.php"
	}
	body, status, err := d.fetcher.FetchRaw(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	matches := clbugURLRegex.FindAllStringSubmatch(string(body), -1)
	limit := d.cfg.Clbug.MaxCandidates
	if limit <= 0 {
		limit = 120
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		candidate := strings.TrimSpace(html.UnescapeString(m[1]))
		if candidate == "" || strings.HasPrefix(candidate, "#") || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LoadConfig 加载发现配置；文件不存在时返回禁用配置（不影响主流程）。
func LoadConfig(path string) (Config, error) {
	cfg := Config{Enabled: false}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	// 配置文件顶层为 discover: 键，与 sources.yaml / whitelist.yaml 结构一致
	var doc struct {
		Discover Config `yaml:"discover"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return cfg, fmt.Errorf("解析发现配置失败: %w", err)
	}
	return doc.Discover, nil
}

// Save 将发现结果写入配置文件（自动生成，供下轮运行合并与审计）。
func Save(path string, sources []model.SourceConfig) error {
	var doc struct {
		Sources []model.SourceConfig `yaml:"sources"`
	}
	doc.Sources = sources
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	header := "# 自动生成的发现源池：由 discover 模块产出，请勿手改。\n" +
		"# 每轮运行自动刷新；仅验证通过（HTTP 200 + 合法 JSON + 有视频列表）的采集站会被收录。\n"
	return os.WriteFile(path, append([]byte(header), data...), 0o644)
}
