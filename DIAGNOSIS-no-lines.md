# 故障诊断：影视软件导入后没有线路

**日期**：2026-11-27
**现象**：部署后把订阅地址导入 TVBox / 影视仓 / FongMi，线路列表为空；原项目 `25175/tvyuan` 同一类客户端可正常加载。
**状态**：已按方案 B 修复完成（2026-11-27），全部测试通过，本地产物已重新生成并校验。

> 修复后校验结果：`sites=50`、唯一 `key=50`、缺失 `key=0`、`type=3` 且缺 `jar=0`；顶层 `spider`/`lives`/`parses` 均已输出。

---

## 1. 直接证据

### 1.1 当前产物 `dist/tvbox.json`

```json
{
  "sites": [
    {
      "name": "360",
      "type": 3,
      "api": "csp_SP360",
      "searchable": 1,
      "quickSearch": 1,
      "spider": "http://home.jundie.top:81/jar/top98_1.jar"
    }
  ]
}
```

对 50 个站点逐项统计（grep 结果）：

| 字段 | 出现次数 | 说明 |
| :--- | ---: | :--- |
| `"key"` | **0** | 全部站点都没有唯一标识 |
| `"jar"` | **0** | 站点级 JAR 字段缺失 |
| `"ext"` | **0** | 爬虫扩展参数缺失 |
| `"spider"`（站点级） | 50 | 写在了客户端不读的字段上 |
| 顶层 `"spider"` | **0** | 全局 JAR 缺失 |
| 顶层 `"lives"` / `"parses"` | 0 | 缺失（次要） |

原项目 `tvbox.json` 中每个站点都有 `key`，且顶层有 `spider`、`lives`、`parses`。

### 1.2 诊断文件 `dist/status.json`

```json
"summary": {
  "total": 244,
  "ok": 0,
  "blocked": 0,
  "failed": 0,
  "ad": 0,
  "spider": 244,
  "whitelist": 0
}
```

含义：244 个源**全部被归类为 spider（爬虫站）**，没有一个采集站通过测速。因此输出里 100% 是 `type=3` 的 csp 站点，**没有任何 type 0/1 的 HTTP 采集 API**。

原项目简洁版全部是 type 0/1 的 HTTP 采集站（`360`、`奶香`、`鸡坤` 等），不需要 JAR，开箱即用。

### 1.3 订阅源实际内容（本次实测）

| 订阅 | 站点数 | type 分布 |
| :--- | ---: | :--- |
| `http://raw.liucn.cc/box/m.json` | 234 | type 1: 15，type 3: 219 |
| `https://ztha.top/TVBox/thdjk.json` | 46 | type 3: 46 |

样本（原订阅）：

```json
{"key":"csp_douban","name":"🌕┃豆瓣┃热播","type":3,"api":"csp_DouDou"}
{"key":"csp_KungFu404","name":"🔥┃设置Token","type":3,"api":"csp_KungFu404","jar":"./libs/jar/Token.jar"}
```

可见上游**本来有 `key`、有 `jar`、有 `ext`**，但本项目在合并过程中把它们丢了。

---

## 2. 根因（按致命程度排序）

### P0-1：站点缺少 `key`，客户端直接丢弃

TVBox / 影视仓的 Site 模型中 `key` 是必填主键（见 `TvBox/docs/CONFIG.md` 与 `fxy1812/tvbox` DeepWiki：`key (String, Required)`）。本项目 `model.Site` 根本没有 `Key` 字段：

`internal/model/tvbox.go:153-173`

```go
type Site struct {
    Name        string   `json:"name"`
    Type        *FlexInt `json:"type"`
    API         string   `json:"api,omitempty"`
    ...
}
```

`UnmarshalJSON` 虽然把 `key` 当作 `name` 的备选来源读取，但**从不输出 `key`**。客户端解析到 50 个没有主键的站点，很可能整批忽略——这解释了“一个线路都没有”，而不是“部分线路不可用”。

### P0-2：JAR 字段用错，所有 csp 站点无法初始化

TVBox 加载 type 3 csp 站点的规则：

1. 优先用站点级 **`jar`** 字段；
2. 没有站点 `jar` 时，用配置顶层 **`spider`** 作为全局 JAR。

本项目把 JAR 写成了站点级 **`spider`**（`site.spider`），TVBox 不读这个字段；顶层 `spider` 又完全缺失（`TVBoxConfig` 只有 `Sites` 一个字段）。结果 50 个 csp 站点全部找不到 JAR，加载失败。

代码位置：

- `internal/model/tvbox.go:195-198`：`TVBoxConfig` 无 `spider` 字段
- `internal/builder/builder.go:147-153`：`s.Spider = src.Spider`（应为 `s.Jar`）
- `internal/fetcher/subscription.go:54-58`：全局 spider 被写到 `s.Spider`，但输出没有顶层 `spider`

### P0-3：上游 `key` / `jar` / `ext` 在合并时丢失

`toSubscriptionSite`（`main.go:431-445`）只传了 `Name`、`API`、`Type`、`Spider`，没有传 `Key`、`Jar`、`Ext`；而 `SourceConfig`（`internal/model/tvbox.go:295-307`）压根没有 `Jar`、`Ext`、`Key` 字段。所以上游订阅里的 `key`、`jar`（如 `./libs/jar/Token.jar`）、`ext` 全部被丢弃。即使修好 P0-2，部分站点仍会因缺 `ext`/专属 JAR 而不可用。

### P1-1：没有任何免 JAR 的采集站

`buildSubscription` 只保留 `StatusOK` 的采集站；`status.json` 显示 `ok=0`，所以采集站全军覆没。剩下全是 csp 爬虫站。原项目能用的前提是简洁版只含 type 0/1 HTTP 采集站。**建议在源池中固定加入若干可直连的采集站 API（可 `skip: true` 或白名单保留），保证即使 JAR 失效也有线路。**

### P1-2：诊断数据大量重复（非致命，但说明去重键有问题）

`status.json` 中同一个 URL（如 `https://ztha.top/TVBox/thdjk.json`）重复几十次。原因：爬虫站的 `URL` 被设成订阅地址，而 `results` map 以 `URL` 为键，导致多条站点共用同一探测结果；`buildStatus` 又逐条追加。应在诊断和输出去重时使用稳定 `key` 或 `name+api` 作为唯一标识。

### P2-1：自家测试无法发现该问题

`main_test.go` 的 `loadConfig` 用同一个 `model.TVBoxConfig` 反序列化产物；因为模型里没有 `key`/`spider` 的校验，测试自然全绿。需要增加“按 TVBox 严格 Schema 校验产物”的测试。

---

## 3. 修复方案（待确认后执行）

### 方案 A（推荐，最小必要修复）

1. `model.Site` 增加并输出 `Key`、`Jar`、`Ext`、`Changeable`、`Filterable`、`Categories` 等客户端已有字段；`UnmarshalJSON` 从上游保留这些字段。
2. `model.TVBoxConfig` 增加顶层 `spider`、`lives`、`parses` 字段并输出（至少 `spider`）。
3. `SourceConfig` 增加 `Key`、`Jar`、`Ext` 字段；`toSubscriptionSite` / `builder.toSite` 完整传递。
4. 取消站点级 `spider` 输出，改为站点级 `jar`；顶层 `spider` 设为出现次数最多的 JAR（与原项目策略一致），并在有 `;md5;` 时保留。
5. `key` 优先用上游原始 `key`；缺失时用 `slug(name)-hash8(api)` 生成稳定值，保证唯一。
6. 增加严格校验测试：每个 site 必须有 `key`；type 3 必须有 `jar` 或顶层 `spider`；顶层必须有 `sites`。

### 方案 B（A + 保底直连源）

在 A 的基础上，向 `config/sources.yaml` 加入若干个 type 0/1 采集站（可直接从原项目当前 `tvbox.json` 取，如 `360zy.com`、`naixxzy.com`、`jkunzyapi.com`），用 `skip: true` 或白名单保留，确保不依赖 JAR 也有线路。

### 方案 C（只修 key，观察）

只补 `key`，不改 JAR 逻辑。风险：type 3 站点仍无法加载，可能依旧没有线路。**不推荐**。

---

## 4. 需要你确认

| # | 决策 | 推荐 |
| :--- | :--- | :--- |
| 1 | 修复范围：A / B / C | **B**（兼容修复 + 保底直连源，最稳） |
| 2 | `key` 生成策略 | 优先保留上游 `key`，缺失时稳定生成 |
| 3 | 全局 JAR 策略 | 取出现次数最多的 JAR 作为顶层 `spider`；站点自有 `jar` 优先 |
| 4 | 是否加入 type 0/1 采集站作为保底 | **加入**（可从原项目取 5~10 个可用 API） |
| 5 | 是否补严格 Schema 测试 | **补**，防止再次回归 |

确认后我按所选方案修改 `internal/model/tvbox.go`、`internal/builder/builder.go`、`internal/fetcher/subscription.go`、`main.go`、`config/sources.yaml` 及测试，并重新生成本地 `dist/tvbox.json` 供你导入验证。
