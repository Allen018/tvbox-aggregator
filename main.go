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
	"log"
	"net/http"
	"os"
	"os/signal"
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

	// ---- 自动发现（一期双通道：GitHub 代码搜索 + 聚合索引站）----
	// 发现源验证通过后与人工源池合并，人工源保持最高信任级
	var discovered []model.SourceConfig
	discoverCfg, err := discover.LoadConfig(discoverPath)
	if err != nil {
		return err
	}
	prevDiscovered := loadDiscovered(discoveredOut)
	if discoverCfg.Enabled {
		existing := make(map[string]bool, len(sources)+len(whitelist)+len(prevDiscovered))
		for _, s := range sources {
			existing[s.URL] = true
		}
		for _, e := range whitelist {
			existing[e.URL] = true
		}
		for _, p := range prevDiscovered {
			existing[p.URL] = true
		}
		disc := discover.New(discoverCfg, f, os.Getenv("GITHUB_TOKEN"))
		discovered, err = disc.Run(ctx, existing, prevDiscovered)
		if err != nil {
			log.Printf("自动发现失败（沿用上轮发现池）: %v", err)
			discovered = prevDiscovered
		}
		if err := discover.Save(discoveredOut, discovered); err != nil {
			log.Printf("发现源池写入失败: %v", err)
		}
	} else {
		// 发现禁用时沿用上轮发现池
		discovered = prevDiscovered
	}

	// 合并源池：人工源在前，发现源补充在后
	allSources := append(sources, discovered...)
	log.Printf("加载源池 %d 个（人工采集站 %d / 爬虫站 %d，发现源 %d），白名单 %d 个",
		len(allSources), countCollect(sources), countSpider(sources), len(discovered), len(whitelist))

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
	if err := b.Build(allSources, results, whitelist, probes, discoveredSet, topN); err != nil {
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
	base.Score = model.Score(base.SpeedMBps, base.TTFBMs)
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
			probes[src.URL] = res
		}(src)
	}
	wg.Wait()
	return probes
}

// probeWhitelist 对白名单源做低成本存活探测（GET 首页/API）。
// 海外返回 403/503 属于白名单预期内（国内可直连），不计为失效。
func probeWhitelist(ctx context.Context, f *fetcher.Fetcher, whitelist []model.WhitelistEntry) map[string]*model.WhitelistProbe {
	probes := make(map[string]*model.WhitelistProbe, len(whitelist))
	var wg sync.WaitGroup
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
			probes[e.URL] = p
		}(e)
	}
	wg.Wait()
	return probes
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
