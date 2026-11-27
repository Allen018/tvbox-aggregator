# tvbox-aggregator

高性能 TVBox 聚合订阅源系统：**Go 高并发测速引擎 + GitHub Actions 定时调度 + Cloudflare Pages 全球边缘分发**，零服务器成本，输出带真实测速与画质标签的多版本订阅配置。

## 特性

- **全网自动发现**：GitHub 代码搜索 + 社区聚合索引站双通道自动发现新采集站，验证通过后分层入池，产物标注 `[新发现]`
- **极速测速**：100 协程并发，Range 512KB 分片探测（固定流量/源），200+ 源全量测速典型 20~40 秒完成
- **强类型防崩**：Go 编译期类型检查 + 自定义宽容 JSON 解析，上游脏数据不中断流水线
- **纯净观影**：m3u8 递归解析、切片广告特征识别（降权不剔除）、预告片自动跳过
- **双轨分发**：海外可测源按实测评分排序；国内保底白名单免测置顶，标注 `[国内直连]`
- **防跑路**：spider jar 本地托管 + 配置直链重写，Cloudflare 自定义域名毫秒级响应
- **可诊断**：每轮输出 `dist/status.json`，全量测速与存活探测结果一目了然

## 目录结构

```text
tvbox-aggregator/
├── .github/workflows/
│   └── auto-update.yml        # 每日定时调度、Go 缓存、产物自动提交
├── config/
│   ├── sources.yaml           # 人工维护源池（采集站 API + 爬虫站）
│   ├── whitelist.yaml         # 国内专属保底源白名单
│   ├── discover.yaml          # 自动发现配置（通道开关与限额）
│   └── discovered.yaml        # 自动生成的发现源池（勿手改，每轮刷新）
├── internal/
│   ├── model/                 # 强类型数据结构与宽容解析
│   ├── fetcher/               # 并发抓取器（连接池复用、自定义请求头、脏数据隔离）
│   ├── tester/                # m3u8 递归解析、广告识别、Range 测速器
│   ├── discover/              # 全网发现：双通道候选收集、端点验证、分层入池
│   └── builder/               # 结果清洗、智能打标、JAR 本地化、多版本产物
├── dist/                      # 自动生成的产物（tvbox.json 单一订阅版 / status.json 诊断）
├── jar/                       # 本地托管的 spider.jar（防上游失效与篡改）
├── go.mod
├── main.go
└── README.md
```

## 本地运行

需要 Go 1.22+：

```bash
go build -o tvbox-aggregator .
./tvbox-aggregator -sources config/sources.yaml -whitelist config/whitelist.yaml -out dist
```

> Windows cmd 下若日志中文显示乱码，先执行 `chcp 65001` 切换到 UTF-8 代码页即可（不影响产物文件，产物均为 UTF-8）。

可选参数：

| 参数 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `-sources` | `config/sources.yaml` | 源池配置文件 |
| `-whitelist` | `config/whitelist.yaml` | 白名单配置文件 |
| `-discover` | `config/discover.yaml` | 自动发现配置（不存在则跳过发现） |
| `-discovered-out` | `config/discovered.yaml` | 发现源池输出路径 |
| `-out` | `dist` | 产物输出目录 |
| `-jar-base` | 环境变量 `TVBOX_JAR_BASE_URL` | 本地托管 jar 的 CF 基础地址（如 `https://你的域名/jar`），为空则不重写 |

GitHub 代码搜索通道需要认证：本地运行前设置环境变量 `GITHUB_TOKEN`（未设置时该通道自动跳过，不影响其余流程）。

## 订阅直链

部署到 Cloudflare Pages 并绑定自定义域名后（见下文），将以下地址复制到 TVBox / 影视仓 / FongMi 客户端的「订阅地址」：

| 版本 | 主直链（Pages 自定义域名） | 兜底直链（jsDelivr） |
| :--- | :--- | :--- |
| 单一订阅版 | `https://你的域名/dist/tvbox.json` | `https://cdn.jsdelivr.net/gh/<用户>/<仓库>@main/dist/tvbox.json` |

**单一订阅版规则**（`dist/tvbox.json`）：

- **数量**：固定收录 **50 条**源（`config/sources.yaml` 的 `top_n` 可调）
- **清洗**：自动剔除测速失败/海外封锁源（无有效速度）、含广告源（体验不佳）；**内容类别排除**——排除教育、网盘、磁力、相声、小品、音乐、戏曲、体育、央视/直播、工具/存储等非影视点播内容，通用影视源予以保留；白名单源仅保留存活探测通过者；爬虫站免测保留
- **排序与名额**：白名单源（`[国内直连]`，国内可用保证）置顶，爬虫站保底（**按订阅组轮询交错，保证来源多样性，不全部来自同一订阅**），测速成功源按综合评分（**实测带宽 × 0.7 + 响应速度 × 0.3**）降序填充剩余名额
- **标注**：每个源带状态标签（`🚀 5.4MB/s` / `[国内直连]` / `[新发现]` 等），一眼识别质量

## 部署步骤

### 1. GitHub 仓库

1. 新建公开仓库（公开仓库 Actions 分钟数免费），推送本工程代码。
2. 进入 `Settings -> Actions -> General -> Workflow permissions`，勾选 **Read and write permissions**（工作流自动提交产物所需）。
3. （可选）`Settings -> Environments and variables -> Variables` 添加 `TVBOX_JAR_BASE_URL = https://你的域名/jar`，用于产物中 jar 地址的本地化重写。

### 2. Cloudflare Pages

1. Cloudflare 控制台 → **Workers & Pages → Create → Pages → Connect to Git**，选择该仓库。
2. 构建配置保持默认（构建命令留空，输出目录 `/`）。
3. **绑定自定义域名**（`*.pages.dev` 免费域名在国内可能被间歇干扰，自定义域名是可达性关键）。
4. 每次 GitHub Actions 更新 `dist/` 后，Pages 将在约 10 秒内自动同步至全球 CDN。

### 3. jar 本地托管

将确认可自由分发的 spider jar 放入仓库 `jar/` 目录并推送，全量版与多仓版中的爬虫源 `spider` 字段会自动重写为 `TVBOX_JAR_BASE_URL` 下的本地地址，防上游失效与篡改。

## 配置说明

### config/sources.yaml

| 字段 | 必填 | 说明 |
| :--- | :--- | :--- |
| `top_n` | 否 | 单一订阅版收录数量，默认 25 |
| `name` | 是 | 站点名称 |
| `url` | 是 | 采集站 API 地址、爬虫站主页，或订阅式源的配置地址 |
| `api` | 否 | 站点 api 地址（采集站 API / 爬虫站 spider 名或 JS 地址）；缺省用 url |
| `type` | 是 | `collect`=采集站（测速参与排序）；`spider`=爬虫站（免测，订阅中保底保留）；`subscription`=订阅式源（抓取配置提取内部站点合并入池，组本身不输出） |
| `category` | 否 | 分组标注（预留） |
| `spider` | 爬虫站必填 | spider jar 原始地址 |
| `headers` | 否 | 按站点自定义请求头（反爬校验，如 `Referer`/`Cookie`） |
| `skip` | 否 | 跳过测速（仍参与配置输出） |
| `local_jar` | 否 | 爬虫站 spider 地址是否重写为本地托管（需 jar 已放入仓库 jar/ 目录；订阅提取的第三方 jar 不重写） |

### config/whitelist.yaml

命中白名单的采集站**免测速**，仅做存活探测（GET 首页/API），自动标注 `[国内直连]` 并置顶。海外返回 403/503 属预期内（国内可直连），不计为失效；网络层失败才计为疑似失效。

### config/discover.yaml（全网自动发现）

| 字段 | 说明 |
| :--- | :--- |
| `enabled` | 发现总开关，关闭时沿用上轮发现池 |
| `max_sources` | 每轮最多收录的发现源数量，控制测速耗时 |
| `github.enabled` / `github.queries` / `github.per_page` | GitHub 代码搜索通道（官方 Search API，需 `GITHUB_TOKEN`），按关键词逐条搜索公开仓库中的采集 API 地址 |
| `index_sites` | 社区聚合索引站列表，抓取页面并提取其中的采集站链接（按需维护） |

**发现与入池机制**：双通道收集候选 → 去重并排除人工源/白名单 → 探测标准 API 端点验证（要求 HTTP 200 + 合法 JSON + 有视频列表）→ 与上轮发现池合并复验（淘汰失效源）→ 验证通过的源写入 `config/discovered.yaml` 并参与本轮测速，产物中标注 `[新发现]`。

**合规边界**：仅探测公开 HTTP 端点，不突破反爬、不绕过 robots、不访问需认证内容；全局限速、低频率运行。搜索引擎结果页不做抓取（如需搜索引擎通道，应走官方 Search API 而非解析结果页）。

### 定时调度

`auto-update.yml` 每日 UTC 0 点（北京时间早 8 点）自动执行一次，另支持 `workflow_dispatch` 手动触发。如需调整频率，修改 cron 即可（GitHub Actions cron 最小粒度为 1 小时）。

## 日常维护

1. **巡检**：每月查看一次 `dist/status.json`，核对失效/海外封锁源，更新 `config/sources.yaml`、`config/whitelist.yaml` 或索引站列表。
2. **源池更新**：人工源直接追加到 `sources.yaml`；自动发现源由 `discover` 模块每轮刷新至 `config/discovered.yaml`，失效源会在复验中自动淘汰。可将稳定发现源提升为人工源（复制到 `sources.yaml`）以获得最高信任级。
3. **发现通道**：GitHub 通道依赖 `GITHUB_TOKEN`（CI 中自动注入，本地需自行设置）；索引站通道按 `discover.yaml` 维护，失效的索引站会自动跳过。
4. **合规安全**：仓库公开时，切勿在配置或代码中硬编码个人隐私、付费账号 Token 或私有代理密码；`GITHUB_TOKEN` 在 CI 中通过 `github.token` 注入，无需写入仓库。

## 技术说明

- **测速口径**：跳过首分片（防片头干扰），取 2~3 个有效分片各测一次取最优；记录 TTFB 与 512KB 吞吐，综合评分 `带宽 × 0.7 + 响应速度 × 0.3`。
- **Range 兼容**：分片小于 512KB 返回 416 时自动降级全量读取；服务端忽略 Range 返回 200 时截断读取。
- **广告识别**：`#EXT-X-DISCONTINUITY` 断点、分片域名突变、分片路径广告特征词，命中后降权打标，不剔除源。
- **订阅清洗与排序**：单一订阅版自动剔除失效/封锁源、含广告源；白名单源（存活探测通过）置顶，爬虫站保底保留（**按订阅组轮询交错保证来源多样性**），测速成功源按综合评分降序填充剩余名额，总截取前 `top_n` 个。
- **订阅式源提取**：抓取订阅配置（兼容 `sites` 站点配置与 `urls` 地址列表两种格式，自动处理 GBK 编码与顶部 `//` 注释），提取内部站点跨订阅合并去重（爬虫站按 api+spider 去重），一层展开子订阅地址；订阅地址本身做延迟探测用于排序与诊断。
- **爬虫站免测与延迟探测**：爬虫源播放地址需客户端侧 spider jar 解析，服务端无法获取真实 m3u8，故不参与测速；订阅中保底保留（排在速度源之后），并按**延迟探测（TTFB）升序**排列（与原项目 `_lat` 排序对齐），未探测到延迟者排最后；spider 地址自动重写为本地托管 jar，探测结果纳入 `status.json` 诊断。

## 测试

```bash
go test ./...
go vet ./...
```
