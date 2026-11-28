// tvbox-aggregator 高性能 TVBox 聚合源系统主入口。
//
// 流程：并发抓取采集站 API → m3u8 递归解析与广告识别 → Range 分片测速 →
// 结果清洗打标 → 输出多版本订阅配置与诊断文件。
//
// 调度策略：
//   - 采集站：全量测速（并发 100，单源 5s 熔断）；
//   - 爬虫站：免测（播放地址需客户端侧 spider jar 解析，服务端无法测速）；
//   - 白名单源：免测速，仅做存活探测并置顶标注 [国内直连]。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"tvbox-aggregator/internal/builder"
	"tvbox-aggregator/internal/discover"
	"tvbox-aggregator/internal/fetcher"
	"tvbox-aggregator/internal/model"
	"tvbox-aggregator/internal/tester"
)

const (
	// concurrency 测速并发度：百级协程，200+ 源全量测速控制在数十秒内。
	concurrency = 100
	// fetchTimeout 单次抓取超时。
	fetchTimeout = 10 * time.Second
	// probeTimeout 单源测速熔断超时。
	probeTimeout = 5 * time.Second
	// probeSamples 每个源提取的 m3u8 候选数。
	probeSamples = 3
)

func main() {
	sourcesPath := flag.String("sources", "config/sources.yaml", "源池配置文件路径")
	whitelistPath := flag.String("whitelist", "config/whitelist.yaml", "国内保底源白名单路径")
	discoverPath := flag.String("discover", "config/discover.yaml", "自动发现配置路径（不存在则跳过发现）")
	discoveredOut := flag.String("discovered-out", "config/discovered.yaml", "发现源池输出路径")
	outDir := flag.String("out", "dist", "产物输出目录")
	jarBaseURL := flag.String("jar-base", os.Getenv("TVBOX_JAR_BASE_URL"), "本地托管 jar 的基础地址（env: TVBOX_JAR_BASE_URL，为空则不重写）")
	flag.Parse()

	if err := run(*sourcesPath, *whitelistPath, *discoverPath, *discoveredOut, *outDir, *jarBaseURL); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

func run(sourcesPath, whitelistPath, discoverPath, discoveredOut, outDir, jarBaseURL string) error {
	// 优雅退出：收到中断信号后停止调度新任务，等待在途任务收尾
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sources, topN, err := loadSources(sourcesPath)
	if err != nil {
		return err
	}
	whitelist, err := loadWhitelist(whitelistPath)
	if err != nil {
		return err
	}

	f := fetcher.New(fetchTimeout)
	prober := tester.NewProber(f.Client(), probeTimeout)

	// ---- 自动发现（GitHub 搜索 + clbug 聚合页 + 索引站）----
	// 发现源经观察期（连续 2 轮成功）才晋升，人工源保持最高信任级
	var discovered []model.SourceConfig
	discoverCfg, err := discover.LoadConfig(discoverPath)
	if err != nil {
		return err
	}
	prevPool := loadDiscovered(discoveredOut)
	if discoverCfg.Enabled {
		existing := make(map[string]bool, len(sources)+len(whitelist)+len(prevPool))
		for _, s := range sources {
			existing[s.URL] = true
		}
		for _, e := range whitelist {
			existing[e.URL] = true
		}
		for _, p := range prevPool {
			existing[p.URL] = true
		}
		disc := discover.New(discoverCfg, f, os.Getenv("GITHUB_TOKEN"))
		var pool []model.SourceConfig
		discovered, pool, err = disc.Run(ctx, existing, prevPool)
		if err != nil {
			log.Printf("自动发现失败（沿用上轮发现池）: %v", err)
			discovered, pool = promotedOnly(prevPool), prevPool
		}
		if err := discover.Save(discoveredOut, pool); err != nil {
			log.Printf("发现源池写入失败: %v", err)
		}
	} else {
		discovered = promotedOnly(prevPool)
	}

	// ---- 订阅式源处理：抓取配置 → 提取站点 → 合并入池 ----
	extractedSources, subProbes := processSubscriptions(ctx, f, sources)

	// 过滤订阅式源（组本身不输出，仅用于提取站点）
	var poolSources []model.SourceConfig
	for _, src := range sources {
		if src.Type != "subscription" {
			poolSources = append(poolSources, src)
		}
	}

	// 合并源池：人工源 + 订阅提取站点 + 发现源
	allSources := append(append(poolSources, extractedSources...), discovered...)
	log.Printf("加载源池 %d 个（人工采集站 %d / 爬虫站 %d，订阅提取 %d，发现源 %d），白名单 %d 个",
		len(allSources), countCollect(poolSources), countSpider(poolSources), len(extractedSources), len(discovered), len(whitelist))

	// 剔除依赖文件已 404 的爬虫站，并记录 JAR 延迟供全局 spider 优选
	artifacts := probeArtifacts(ctx, f, allSources)
	before := len(allSources)
	kept := make([]model.SourceConfig, 0, before)
	for _, src := range allSources {
		if src.Type == "spider" {
			if info := artifacts[artifactURL(src.Jar)]; info != nil {
				if info.Dead {
					continue
				}
				src.JarTTFB = info.TTFBMs
			}
			if info := artifacts[artifactURL(src.API)]; info != nil && info.Dead {
				continue
			}
		}
		kept = append(kept, src)
	}
	if removed := before - len(kept); removed > 0 {
		log.Printf("依赖文件 404，剔除爬虫站 %d 个", removed)
	}
	allSources = kept

	// 白名单 URL 集合：命中白名单的采集站不做全量测速，改为存活探测
	wlURLs := make(map[string]model.WhitelistEntry, len(whitelist))
	for _, e := range whitelist {
		wlURLs[e.URL] = e
	}

	results := make(map[string]*model.TestResult, len(allSources))

	// ---- 测速 Worker Pool ----
	jobs := make(chan model.SourceConfig, len(allSources))
	resCh := make(chan *model.TestResult, len(allSources))

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				resCh <- testSource(ctx, f, prober, src)
			}
		}()
	}

	// 爬虫站延迟探测（并发 TTFB 探测，用于订阅内排序）
	var spiderSources []model.SourceConfig
	for _, src := range allSources {
		if src.Type == "spider" {
			spiderSources = append(spiderSources, src)
		}
	}
	spiderProbes := probeSpiderLatency(ctx, f, spiderSources)

	for _, src := range allSources {
		if src.Type == "spider" {
			// 爬虫站：免测速，使用延迟探测结果
			if p, ok := spiderProbes[src.URL]; ok && p != nil {
				resCh <- p
			} else {
				resCh <- &model.TestResult{
					Name: src.Name, URL: src.URL, Status: model.StatusUnknown,
					CheckedAt: time.Now().UTC(),
				}
			}
			continue
		}
		if src.Skip || isWhitelisted(src, wlURLs) {
			// 跳过源 / 白名单源：免测速，直接记录
			resCh <- &model.TestResult{
				Name: src.Name, URL: src.URL, Status: model.StatusUnknown,
				CheckedAt: time.Now().UTC(),
			}
			continue
		}
		jobs <- src
	}
	close(jobs)
	wg.Wait()
	close(resCh)
	for r := range resCh {
		results[r.URL] = r
	}

	// ---- 白名单存活探测 ----
	probes := probeWhitelist(ctx, f, whitelist)

	// ---- 构建产物 ----
	discoveredSet := make(map[string]bool, len(discovered))
	for _, s := range discovered {
		discoveredSet[s.URL] = true
	}
	b := builder.New(jarBaseURL, outDir)
	if err := b.Build(allSources, results, whitelist, probes, discoveredSet, topN, subProbes); err != nil {
		return err
	}

	printSummary(allSources, results, whitelist, probes)
	return nil
}

// testSource 执行单源完整测速：拉取列表 → 提取 m3u8 → 解析与测速 → 组装结果。
func testSource(ctx context.Context, f *fetcher.Fetcher, prober *tester.Prober, src model.SourceConfig) *model.TestResult {
	base := &model.TestResult{
		Name: src.Name, URL: src.URL, Status: model.StatusFailed, CheckedAt: time.Now().UTC(),
	}

	vl, err := f.FetchVodList(ctx, src.URL, src.Headers)
	if err != nil {
		base.Status = classify(err)
		base.Error = err.Error()
		return base
	}

	// 新鲜度：取列表中最新的 vod_time；超过 60 天直接判为陈旧源，不再浪费播放测速。
	freshDays := -1
	var newest time.Time
	for _, item := range vl.List {
		t := model.ParseVodTime(item.Time)
		if !t.IsZero() && t.After(newest) {
			newest = t
		}
	}
	if !newest.IsZero() {
		d := int(time.Since(newest).Hours() / 24)
		if d < 0 {
			d = 0
		}
		freshDays = d
		base.FreshDays = d
		if d > 60 {
			base.Status = model.StatusStale
			base.Error = fmt.Sprintf("影片资源陈旧，最新影片为 %d 天前", d)
			return base
		}
	}

	// 列表接口常不带播放地址（只有 vod_id），回退 ac=detail 补齐后再提取 m3u8
	if len(fetcher.ExtractPlayURLs(vl, 1)) == 0 {
		if detailed := f.FetchVodDetail(ctx, src.URL, src.Headers, vl.List); len(detailed) > 0 {
			vl.List = detailed
		}
	}

	playURLs := fetcher.ExtractPlayURLs(vl, probeSamples)
	if len(playURLs) == 0 {
		base.Error = "未提取到 m3u8 播放地址"
		return base
	}

	// 依次尝试候选 m3u8，任一成功即采用（不同影片互为备份）
	var ana *tester.AnalyzeResult
	var analyzeErr error
	for _, u := range playURLs {
		ana, analyzeErr = prober.Analyze(ctx, u, src.Headers)
		if analyzeErr == nil {
			break
		}
	}
	if analyzeErr != nil {
		base.Status = classify(analyzeErr)
		base.Error = analyzeErr.Error()
		return base
	}

	base.Status = model.StatusOK
	base.SpeedMBps = ana.Probe.SpeedMBps
	base.TTFBMs = ana.Probe.TTFBMs
	base.RangeOK = ana.Probe.RangeOK
	base.Samples = ana.Samples
	if ana.Ad.Suspicious() {
		// 含广告：降权保留，不剔除
		base.Status = model.StatusAd
		base.AdTag = true
	}
	base.Score = model.Score(model.ScoreInput{
		SpeedMBps: base.SpeedMBps,
		TTFBMs:    base.TTFBMs,
		FreshDays: freshDays,
		Stability: 1,
		Ad:        base.AdTag,
		JarOK:     true, // 采集站不依赖 JAR
	})
	return base
}

// classify 将测速错误归类为状态：403/503 为海外封锁，其余为失败。
// 抓取与测速阶段均返回 *model.HTTPStatusError，分类口径一致。
func classify(err error) string {
	var he *model.HTTPStatusError
	if errors.As(err, &he) {
		if he.Status == http.StatusForbidden || he.Status == http.StatusServiceUnavailable {
			return model.StatusBlocked
		}
	}
	return model.StatusFailed
}

// probeSpiderLatency 对爬虫站做延迟探测（GET 站点地址，测量 TTFB），
// 用于订阅内爬虫站按延迟升序排序。海外 403/503 属预期内，标记封锁但保留。
func probeSpiderLatency(ctx context.Context, f *fetcher.Fetcher, spiders []model.SourceConfig) map[string]*model.TestResult {
	probes := make(map[string]*model.TestResult, len(spiders))
	var wg sync.WaitGroup
	var mu sync.Mutex              // 并发写 map 必须加锁，否则 runtime 会 fatal error: concurrent map writes
	sem := make(chan struct{}, 20) // 并发收敛，爬虫站数量少、成本极低
	for _, src := range spiders {
		wg.Add(1)
		sem <- struct{}{}
		go func(src model.SourceConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			res := &model.TestResult{
				Name: src.Name, URL: src.URL, Status: model.StatusFailed,
				CheckedAt: time.Now().UTC(),
			}
			start := time.Now()
			body, status, err := f.FetchRaw(pctx, src.URL, src.Headers)
			res.TTFBMs = int64(time.Since(start).Milliseconds())
			if res.TTFBMs <= 0 {
				// 时钟精度兜底：本地回环响应可能低于时钟粒度，记 1ms
				res.TTFBMs = 1
			}
			switch {
			case err != nil:
				res.Error = err.Error() // 网络层失败：源大概率已失效
			case status == http.StatusOK && len(body) > 0:
				res.Status = model.StatusOK
			case status == http.StatusForbidden || status == http.StatusServiceUnavailable:
				res.Status = model.StatusBlocked
				res.Error = "海外 IP 封锁（预期内，国内可直连）"
			default:
				res.Error = fmt.Sprintf("HTTP %d，响应异常", status)
			}
			mu.Lock()
			probes[src.URL] = res
			mu.Unlock()
		}(src)
	}
	wg.Wait()
	return probes
}

// processSubscriptions 处理全部订阅式源：并发抓取配置、提取站点、一层展开子订阅地址。
// 返回合并去重后的站点列表与各订阅的诊断结果。订阅式源本身不输出，仅作为站点提取入口。
func processSubscriptions(ctx context.Context, f *fetcher.Fetcher, sources []model.SourceConfig) ([]model.SourceConfig, map[string]*model.SubscriptionProbe) {
	var subSources []model.SourceConfig
	for _, src := range sources {
		if src.Type == "subscription" {
			subSources = append(subSources, src)
		}
	}
	if len(subSources) == 0 {
		return nil, map[string]*model.SubscriptionProbe{}
	}

	type subResult struct {
		src   model.SourceConfig
		sites []model.SourceConfig
		urls  []string
		probe *model.SubscriptionProbe
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	resCh := make(chan subResult, len(subSources))
	for _, src := range subSources {
		wg.Add(1)
		sem <- struct{}{}
		go func(src model.SourceConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			sites, urls, probe := processSubscription(ctx, f, src)
			resCh <- subResult{src: src, sites: sites, urls: urls, probe: probe}
		}(src)
	}
	wg.Wait()
	close(resCh)

	extracted := make([]model.SourceConfig, 0, 64)
	seen := map[string]bool{}
	subProbes := map[string]*model.SubscriptionProbe{}
	var childURLs []string
	for r := range resCh {
		subProbes[r.src.URL] = r.probe
		for _, s := range r.sites {
			addExtracted(&extracted, seen, s)
		}
		childURLs = append(childURLs, r.urls...)
	}

	// 一层展开子订阅地址（限量，防失控）
	extracted = append(extracted, expandSubscriptionURLs(ctx, f, childURLs, seen)...)
	return extracted, subProbes
}

// processSubscription 抓取单个订阅式源配置，提取站点与子订阅地址。
func processSubscription(ctx context.Context, f *fetcher.Fetcher, src model.SourceConfig) ([]model.SourceConfig, []string, *model.SubscriptionProbe) {
	probe := &model.SubscriptionProbe{
		Name: src.Name, URL: src.URL, Status: model.StatusFailed, CheckedAt: time.Now().UTC(),
	}
	start := time.Now()
	res, err := f.FetchSubscription(ctx, src.URL, src.Headers)
	probe.TTFBMs = int64(time.Since(start).Milliseconds())
	if err != nil {
		probe.Error = err.Error()
		return nil, nil, probe
	}
	probe.Status = model.StatusOK
	probe.Sites = len(res.Sites)
	probe.URLs = len(res.URLs)

	sites := make([]model.SourceConfig, 0, len(res.Sites))
	for _, s := range res.Sites {
		sites = append(sites, toSubscriptionSite(s, src.URL))
	}
	return sites, res.URLs, probe
}

// expandSubscriptionURLs 一层展开订阅地址列表中的子订阅地址，提取站点（限量 20 个）。
func expandSubscriptionURLs(ctx context.Context, f *fetcher.Fetcher, urls []string, seen map[string]bool) []model.SourceConfig {
	unique := map[string]bool{}
	for _, u := range urls {
		if u != "" {
			unique[u] = true
		}
	}
	out := make([]model.SourceConfig, 0, 32)
	count := 0
	for u := range unique {
		if count >= 20 {
			break
		}
		count++
		res, err := f.FetchSubscription(ctx, u, nil)
		if err != nil {
			continue
		}
		for _, s := range res.Sites {
			addExtracted(&out, seen, toSubscriptionSite(s, u))
		}
	}
	return out
}

// addExtracted 将订阅提取的站点加入列表并去重：
// 采集站按名称+地址去重；爬虫站按 api+spider 去重（相同配置的站点视为同一源，
// 避免同一订阅的冗余站点挤占名额、降低多样性）。
func addExtracted(dst *[]model.SourceConfig, seen map[string]bool, src model.SourceConfig) {
	if src.Name == "" || src.URL == "" {
		return
	}
	// 优先以客户端主键 key 去重；缺失时退化为 name+url，爬虫站再附加 jar 区分。
	key := src.Key + "|" + src.API
	if src.Key == "" {
		key = src.Name + "|" + src.URL
	}
	if src.Type == "spider" {
		key += "|" + src.Jar
	}
	if seen[key] {
		return
	}
	seen[key] = true
	*dst = append(*dst, src)
}

// toSubscriptionSite 将订阅中解析出的站点转为源池条目。
// 爬虫站：API 保留原始 spider 名/JS 地址（客户端使用），URL 设为订阅地址（延迟探测目标）；
// 采集站：API 与 URL 均为采集站 API 地址。
// Key/Jar/Ext/Categories 与 OutputType 一并保留，避免客户端所需字段在合并中丢失。
func toSubscriptionSite(s model.Site, subURL string) model.SourceConfig {
	outType := 0
	if s.Type != nil && s.Type.Set {
		outType = s.Type.Value
	}
	src := model.SourceConfig{
		Name:       s.Name,
		Key:        s.Key,
		URL:        s.API,
		API:        s.API,
		Type:       "collect",
		OutputType: outType,
		Jar:        s.Jar,
		Ext:        s.Ext,
		Categories: s.Categories,
	}
	// 只有 type 3（csp/JAR/JS）才是爬虫站；type 0/1 是标准采集 API，必须走真实测速。
	if s.Type != nil && s.Type.Value == 3 {
		src.Type = "spider"
		src.URL = subURL
	}
	return src
}

// probeWhitelist 对白名单源做低成本存活探测（GET 首页/API）。
// 海外返回 403/503 属于白名单预期内（国内可直连），不计为失效。
func probeWhitelist(ctx context.Context, f *fetcher.Fetcher, whitelist []model.WhitelistEntry) map[string]*model.WhitelistProbe {
	probes := make(map[string]*model.WhitelistProbe, len(whitelist))
	var wg sync.WaitGroup
	var mu sync.Mutex              // 并发写 map 必须加锁
	sem := make(chan struct{}, 20) // 白名单探测并发收敛，成本极低
	for _, e := range whitelist {
		wg.Add(1)
		sem <- struct{}{}
		go func(e model.WhitelistEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			p := &model.WhitelistProbe{Name: e.Name, URL: e.URL, CheckedAt: time.Now().UTC()}
			body, status, err := f.FetchRaw(pctx, e.URL, e.Headers)
			p.Status = status
			switch {
			case err != nil:
				p.Error = err.Error() // 网络层失败：源大概率已失效
			case status == http.StatusOK && len(body) > 0:
				p.Alive = true
			case status == http.StatusForbidden || status == http.StatusServiceUnavailable:
				p.Alive = true
				p.Error = "海外 IP 封锁（白名单预期内，国内可直连）"
			default:
				p.Error = fmt.Sprintf("HTTP %d，响应异常（可能被 WAF 拦截或源已失效）", status)
			}
			mu.Lock()
			probes[e.URL] = p
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	return probes
}

// artifactURL 提取爬虫站依赖的 jar/js/py 文件地址（去掉 ;md5; 后缀）。
// csp_/py_ 类名与相对路径不在此检查（相对路径已在订阅解析阶段绝对化）。
func artifactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "csp_") || strings.HasPrefix(raw, "py_") {
		return ""
	}
	if i := strings.Index(raw, ";"); i >= 0 {
		raw = raw[:i]
	}
	if !strings.Contains(raw, "://") {
		return ""
	}
	return raw
}

// artifactInfo 依赖文件的探测结果：Dead 表示确定 404/410，TTFBMs 用于优选全局 JAR。
type artifactInfo struct {
	Dead   bool
	TTFBMs int64
}

// probeArtifacts 探测爬虫站依赖的 jar/js/py 文件是否真实存在，并记录响应延迟。
// 仅对确定的 404/410 判死；403/5xx/超时一律保留（可能是海外封锁或临时故障）。
func probeArtifacts(ctx context.Context, f *fetcher.Fetcher, sources []model.SourceConfig) map[string]*artifactInfo {
	candidates := map[string]bool{}
	for _, s := range sources {
		if s.Type != "spider" {
			continue
		}
		if u := artifactURL(s.Jar); u != "" {
			candidates[u] = true
		}
		if u := artifactURL(s.API); u != "" {
			candidates[u] = true
		}
	}
	infos := make(map[string]*artifactInfo, len(candidates))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 10)
	for u := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()

			dead, ttfb := false, int64(9999)
			req, err := http.NewRequestWithContext(pctx, http.MethodHead, u, nil)
			if err == nil {
				start := time.Now()
				resp, err := f.Client().Do(req)
				if err == nil {
					ttfb = time.Since(start).Milliseconds()
					resp.Body.Close()
					if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
						dead = true
					}
				}
			}
			// HEAD 不可用时用 GET 取前 1KB 测延迟
			if !dead && ttfb >= 9999 {
				req, err := http.NewRequestWithContext(pctx, http.MethodGet, u, nil)
				if err == nil {
					start := time.Now()
					resp, err := f.Client().Do(req)
					if err == nil {
						ttfb = time.Since(start).Milliseconds()
						_, _ = io.CopyN(io.Discard, resp.Body, 1<<10)
						resp.Body.Close()
						if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
							dead = true
						}
					}
				}
			}
			mu.Lock()
			infos[u] = &artifactInfo{Dead: dead, TTFBMs: ttfb}
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return infos
}

// ---------------------------------------------------------------------------
// 配置加载
// ---------------------------------------------------------------------------

// loadSources 加载源池配置，返回源列表与订阅收录数量（top_n，缺省 15）。
func loadSources(path string) ([]model.SourceConfig, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读取源池配置失败: %w", err)
	}
	var cfg struct {
		Sources []model.SourceConfig `yaml:"sources"`
		TopN    int                  `yaml:"top_n"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, 0, fmt.Errorf("解析源池配置失败: %w", err)
	}
	if len(cfg.Sources) == 0 {
		return nil, 0, fmt.Errorf("源池配置为空: %s", path)
	}
	return cfg.Sources, cfg.TopN, nil
}

// loadWhitelist 加载国内保底源白名单。
func loadWhitelist(path string) ([]model.WhitelistEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取白名单配置失败: %w", err)
	}
	var cfg struct {
		Whitelist []model.WhitelistEntry `yaml:"whitelist"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析白名单配置失败: %w", err)
	}
	return cfg.Whitelist, nil
}

// loadDiscovered 加载上轮发现的源池（与 sources.yaml 同格式，自动生成）。
// 文件不存在时返回空，发现模块本轮会重新产出。
func loadDiscovered(path string) []model.SourceConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		Sources []model.SourceConfig `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("发现源池解析失败（忽略，本轮重新发现）: %v", err)
		return nil
	}
	return cfg.Sources
}

// promotedOnly 只保留观察期中已晋升的发现源，避免未观察完成的候选进入产物。
func promotedOnly(sources []model.SourceConfig) []model.SourceConfig {
	out := make([]model.SourceConfig, 0, len(sources))
	for _, s := range sources {
		if s.Promoted {
			out = append(out, s)
		}
	}
	return out
}

func isWhitelisted(src model.SourceConfig, wlURLs map[string]model.WhitelistEntry) bool {
	_, ok := wlURLs[src.URL]
	return ok
}

func countCollect(sources []model.SourceConfig) int {
	n := 0
	for _, s := range sources {
		if s.Type != "spider" {
			n++
		}
	}
	return n
}

func countSpider(sources []model.SourceConfig) int {
	n := 0
	for _, s := range sources {
		if s.Type == "spider" {
			n++
		}
	}
	return n
}

// printSummary 输出本轮聚合统计。
func printSummary(sources []model.SourceConfig, results map[string]*model.TestResult, whitelist []model.WhitelistEntry, probes map[string]*model.WhitelistProbe) {
	var ok, blocked, failed, ad, unknown int
	for _, s := range sources {
		r := results[s.URL]
		if r == nil {
			unknown++
			continue
		}
		switch r.Status {
		case model.StatusOK:
			ok++
		case model.StatusBlocked:
			blocked++
		case model.StatusAd:
			ad++
		case model.StatusFailed:
			failed++
		default:
			unknown++
		}
	}
	alive := 0
	for _, p := range probes {
		if p.Alive {
			alive++
		}
	}
	log.Printf("测速完成: 成功 %d | 含广告 %d | 海外封锁 %d | 失效 %d | 免测 %d；白名单 %d/%d 存活",
		ok, ad, blocked, failed, unknown, alive, len(whitelist))
}
