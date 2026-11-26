package builder

import (
	"fmt"
	"testing"

	"tvbox-aggregator/internal/model"
)

func TestRewriteJar(t *testing.T) {
	b := New("https://cdn.example.com/jar", "dist")
	s := model.Site{
		Name:   "爬虫站",
		Type:   model.IntPtr(1),
		Spider: "https://upstream.example.com/spider/abc.jar",
	}
	got := b.rewriteJar(s)
	if got.Spider != "https://cdn.example.com/jar/abc.jar" {
		t.Errorf("spider 重写错误: %q", got.Spider)
	}

	// 无基础地址时不重写
	b2 := New("", "dist")
	got2 := b2.rewriteJar(s)
	if got2.Spider != s.Spider {
		t.Error("无 JarBaseURL 时不应重写")
	}
}

func TestBuildSubscriptionCleaning(t *testing.T) {
	b := New("", "dist")
	probes := map[string]*model.WhitelistProbe{
		"http://wl-alive.com/api": {Alive: true},
		"http://wl-dead.com/api":  {Alive: false},
	}
	sites := []model.Site{
		{Name: "白名单存活", SourceURL: "http://wl-alive.com/api", Status: model.StatusOK, Tags: []string{"[国内直连]"}},
		{Name: "白名单失效", SourceURL: "http://wl-dead.com/api", Status: model.StatusOK, Tags: []string{"[国内直连]"}},
		{Name: "测速成功", SourceURL: "http://ok.com/api", Status: model.StatusOK, Score: 5.0},
		{Name: "含广告", SourceURL: "http://ad.com/api", Status: model.StatusAd, Score: 9.0},
		{Name: "失效", SourceURL: "http://fail.com/api", Status: model.StatusFailed, Score: 0},
		{Name: "爬虫", SourceURL: "http://spider.com/", Type: model.IntPtr(1)},
	}
	cfg := b.buildSubscription(sites, probes, 15)
	if len(cfg.Sites) != 3 {
		t.Fatalf("期望 3 个站点（白名单存活 + 测速成功 + 爬虫），实际 %d: %+v", len(cfg.Sites), cfg.Sites)
	}
	// 排序：白名单置顶 → 测速成功 → 爬虫站保底
	if cfg.Sites[0].Name != "白名单存活" {
		t.Errorf("首位应为白名单源，实际 %q", cfg.Sites[0].Name)
	}
	if cfg.Sites[1].Name != "测速成功" {
		t.Errorf("次位应为测速成功源，实际 %q", cfg.Sites[1].Name)
	}
	if cfg.Sites[2].Name != "爬虫" {
		t.Errorf("末位应为爬虫站，实际 %q", cfg.Sites[2].Name)
	}
}

func TestBuildSubscriptionSpiderGuaranteed(t *testing.T) {
	b := New("", "dist")
	var ranked []model.Site
	for i := 0; i < 10; i++ {
		ranked = append(ranked, model.Site{
			Name:      fmt.Sprintf("源%d", i),
			SourceURL: fmt.Sprintf("http://r%d.com/api", i),
			Status:    model.StatusOK,
			Score:     float64(10 - i),
		})
	}
	sites := append(ranked, model.Site{
		Name: "爬虫站", SourceURL: "http://spider.com/", Type: model.IntPtr(1),
	})
	// topN=5：爬虫站保底占 1 席，速度源仅取前 4
	cfg := b.buildSubscription(sites, nil, 5)
	if len(cfg.Sites) != 5 {
		t.Fatalf("期望 5 个，实际 %d", len(cfg.Sites))
	}
	if cfg.Sites[4].Name != "爬虫站" {
		t.Errorf("爬虫站应保底保留在末位，实际 %q", cfg.Sites[4].Name)
	}
	if cfg.Sites[0].Score != 10 || cfg.Sites[3].Score != 7 {
		t.Errorf("速度源应按评分降序填充剩余名额: %+v", cfg.Sites)
	}
}

func TestBuildSubscriptionSpiderLatencySort(t *testing.T) {
	b := New("", "dist")
	var ranked []model.Site
	for i := 0; i < 5; i++ {
		ranked = append(ranked, model.Site{
			Name:      fmt.Sprintf("源%d", i),
			SourceURL: fmt.Sprintf("http://r%d.com/api", i),
			Status:    model.StatusOK,
			Score:     float64(5 - i),
		})
	}
	// 四个爬虫站：两个探测成功（按延迟升序）、一个未探测、一个探测失败（均排最后）
	sites := append(ranked,
		model.Site{Name: "爬虫慢", SourceURL: "http://s1.com/", Type: model.IntPtr(1), Status: model.StatusOK, LatencyMs: 800},
		model.Site{Name: "爬虫快", SourceURL: "http://s2.com/", Type: model.IntPtr(1), Status: model.StatusOK, LatencyMs: 120},
		model.Site{Name: "爬虫未探测", SourceURL: "http://s3.com/", Type: model.IntPtr(1)},
		model.Site{Name: "爬虫失败", SourceURL: "http://s4.com/", Type: model.IntPtr(1), Status: model.StatusFailed, LatencyMs: 50},
	)
	cfg := b.buildSubscription(sites, nil, 15)
	if len(cfg.Sites) != 9 {
		t.Fatalf("期望 9 个站点，实际 %d", len(cfg.Sites))
	}
	// 爬虫段按延迟升序：快(120) → 慢(800) → 未探测 → 失败（哨兵最大，稳定序）
	want := []string{"爬虫快", "爬虫慢", "爬虫未探测", "爬虫失败"}
	for i, w := range want {
		if cfg.Sites[5+i].Name != w {
			t.Errorf("第 %d 个爬虫站=%q，期望 %q（完整: %+v）", i, cfg.Sites[5+i].Name, w, cfg.Sites[5:])
		}
	}
}

func TestBuildSubscriptionTopN(t *testing.T) {
	b := New("", "dist")
	var sites []model.Site
	for i := 0; i < 5; i++ {
		sites = append(sites, model.Site{
			Name:      fmt.Sprintf("源%d", i),
			SourceURL: fmt.Sprintf("http://s%d.com/api", i),
			Status:    model.StatusOK,
			Score:     float64(i + 1),
		})
	}
	cfg := b.buildSubscription(sites, nil, 3)
	if len(cfg.Sites) != 3 {
		t.Fatalf("期望截取 3 个，实际 %d", len(cfg.Sites))
	}
	// 按评分降序
	if cfg.Sites[0].Score < cfg.Sites[1].Score || cfg.Sites[1].Score < cfg.Sites[2].Score {
		t.Errorf("应按评分降序排列: %+v", cfg.Sites)
	}
}

func TestBuildSubscriptionDefaultTopN(t *testing.T) {
	b := New("", "dist")
	var sites []model.Site
	for i := 0; i < 20; i++ {
		sites = append(sites, model.Site{
			Name:      fmt.Sprintf("源%d", i),
			SourceURL: fmt.Sprintf("http://s%d.com/api", i),
			Status:    model.StatusOK,
			Score:     float64(i + 1),
		})
	}
	// topN 缺省（0）时使用默认 15
	cfg := b.buildSubscription(sites, nil, 0)
	if len(cfg.Sites) != defaultTopN {
		t.Fatalf("缺省应截取 %d 个，实际 %d", defaultTopN, len(cfg.Sites))
	}
}
