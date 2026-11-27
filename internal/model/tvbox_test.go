package model

import (
	"encoding/json"
	"math"
	"testing"
)

func TestFlexIntUnmarshal(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantVal int
		wantSet bool
	}{
		{"数字", `1`, 1, true},
		{"数字字符串", `"1"`, 1, true},
		{"零字符串", `"0"`, 0, true},
		{"true", `true`, 1, true},
		{"字符串true", `"true"`, 1, true},
		{"false", `false`, 0, true},
		{"null", `null`, 0, false},
		{"非数字字符串", `"abc"`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f FlexInt
			if err := json.Unmarshal([]byte(c.in), &f); err != nil {
				t.Fatalf("Unmarshal 报错: %v", err)
			}
			if f.Set != c.wantSet {
				t.Errorf("Set=%v，期望 %v", f.Set, c.wantSet)
			}
			if f.Set && f.Value != c.wantVal {
				t.Errorf("Value=%d，期望 %d", f.Value, c.wantVal)
			}
		})
	}
}

func TestFlexIntMarshal(t *testing.T) {
	var f FlexInt
	f.Value, f.Set = 3, true
	data, err := json.Marshal(f)
	if err != nil || string(data) != "3" {
		t.Fatalf("Marshal 期望 3，实际 %s (%v)", data, err)
	}
	f.Set = false
	data, err = json.Marshal(f)
	if err != nil || string(data) != "null" {
		t.Fatalf("未设置时 Marshal 期望 null，实际 %s (%v)", data, err)
	}
}

func TestFlexStringUnmarshal(t *testing.T) {
	var s FlexString
	if err := json.Unmarshal([]byte(`"文本"`), &s); err != nil || string(s) != "文本" {
		t.Fatalf("字符串解析失败: %q (%v)", s, err)
	}
	if err := json.Unmarshal([]byte(`123`), &s); err != nil || string(s) != "123" {
		t.Fatalf("数字解析失败: %q (%v)", s, err)
	}
}

func TestSiteUnmarshalHeterogeneous(t *testing.T) {
	data := `{"name":"测试站","type":"1","api":"http://a.com/api","searchable":true,"jar":"http://b.com/a.jar"}`
	var s Site
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		t.Fatalf("Unmarshal 报错: %v", err)
	}
	if s.Name != "测试站" {
		t.Errorf("Name=%q", s.Name)
	}
	if s.Type == nil || s.Type.Value != 1 {
		t.Errorf("type 字符串 1 解析失败: %+v", s.Type)
	}
	if s.API != "http://a.com/api" {
		t.Errorf("API=%q", s.API)
	}
	if s.Searchable == nil || s.Searchable.Value != 1 {
		t.Errorf("searchable bool true 解析失败: %+v", s.Searchable)
	}
	if s.Jar != "http://b.com/a.jar" {
		t.Errorf("Jar=%q", s.Jar)
	}
}

func TestSiteUnmarshalAlternativeKeys(t *testing.T) {
	// 异构字段命名：url 代替 api、key 代替 name
	data := `{"key":"备用名","url":"http://x.com","type":0}`
	var s Site
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		t.Fatalf("Unmarshal 报错: %v", err)
	}
	if s.Name != "备用名" || s.API != "http://x.com" {
		t.Errorf("异构字段解析失败: name=%q api=%q", s.Name, s.API)
	}
}

func TestTVBoxConfigDirtyDataIsolation(t *testing.T) {
	// 站点内字段类型漂移（合法 JSON 但字段脏）：应全部解析成功，脏字段保持零值
	data := `{"sites":[
		{"name":"正常站","type":0,"api":"http://a.com"},
		{"name":"脏字段站","type":["异常","类型"],"api":123,"searchable":"maybe"},
		{"name":"爬虫站","type":1,"spider":"http://c.com/s.jar"}
	]}`
	var c TVBoxConfig
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("Unmarshal 报错: %v", err)
	}
	if len(c.Sites) != 3 {
		t.Fatalf("期望 3 个站点，实际 %d", len(c.Sites))
	}
	if c.Sites[1].Type != nil && c.Sites[1].Type.Set {
		t.Error("脏 type 字段应保持未设置")
	}
	if c.Sites[1].API != "" {
		t.Errorf("脏 api 字段应为空，实际 %q", c.Sites[1].API)
	}
	if c.Sites[2].Spider != "http://c.com/s.jar" {
		t.Errorf("Spider=%q", c.Sites[2].Spider)
	}
}

func TestVodListVariants(t *testing.T) {
	// 容器字段为 data 且 code 为字符串的变体
	data := `{"code":"1","msg":"ok","data":[{"name":"影片","play_url":"第1集$https://a.com/1.m3u8"}]}`
	var vl VodList
	if err := json.Unmarshal([]byte(data), &vl); err != nil {
		t.Fatalf("Unmarshal 报错: %v", err)
	}
	if vl.Code != 1 {
		t.Errorf("code 字符串解析失败: %d", vl.Code)
	}
	if len(vl.List) != 1 || vl.List[0].PlayURL != "第1集$https://a.com/1.m3u8" {
		t.Errorf("data 容器解析失败: %+v", vl.List)
	}
}

func TestScore(t *testing.T) {
	good := Score(ScoreInput{SpeedMBps: 10, TTFBMs: 100, FreshDays: 1, Stability: 1, JarOK: true})
	if good < 60 || good > 100 {
		t.Errorf("优质源评分异常: %f", good)
	}
	bad := Score(ScoreInput{SpeedMBps: 0.1, TTFBMs: 5000, FreshDays: 100, Stability: 0.2, Ad: true})
	if bad >= good {
		t.Errorf("劣质源评分应低于优质源: bad=%f good=%f", bad, good)
	}
	unknown := Score(ScoreInput{})
	if unknown <= 0 {
		t.Errorf("未知维度应按中性值处理，实际 %f", unknown)
	}
	_ = math.Abs
}
