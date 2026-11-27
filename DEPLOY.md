# 部署指南：GitHub + Cloudflare

本文档为 tvbox-aggregator 的完整部署步骤：GitHub 托管代码与定时调度，Cloudflare Pages 做全球 CDN 分发。

## 0. 部署前准备

| 准备项 | 说明 |
| :--- | :--- |
| GitHub 账号 | 用于创建公开仓库与 Actions 调度 |
| Cloudflare 账号 | 免费，用于 Pages 托管与 CDN 加速 |
| 域名（可选但强烈建议） | 用于绑定 Pages 自定义域名，保障国内可达性；无域名可先用 `*.pages.dev` 测试 |
| Git 环境 | 本地已安装 Git，能推送到 GitHub |

---

## 1. GitHub 侧：代码托管 + 定时调度

### 1.1 创建公开仓库

1. 登录 GitHub，点击 **New repository**。
2. 名称填 `tvbox-aggregator`，**必须选择 Public（公开）**。
   - 公开仓库的 GitHub Actions 运行分钟数免费；私有仓库仅 2000 分钟/月。
3. 不勾选 "Add a README"（本仓库自带 README，推送时会一并提交）。
4. 点击 **Create repository**。

### 1.2 推送代码

在项目目录执行：

```bash
cd C:\Users\Administrator.DESKTOP-HEO0D09\Documents\tvbox

git init
git add .
git commit -m "init: tvbox-aggregator 聚合源项目"
git branch -M main
git remote add origin https://github.com/<你的用户名>/tvbox-aggregator.git
git push -u origin main
```

说明：

- `.gitignore` 已排除编译产物 `tvbox-aggregator.exe`，不会误提交。
- 首次推送需输入 GitHub 账号密码（或使用 Personal Access Token）。
- 推送成功后，仓库中应包含 `.github/workflows/auto-update.yml`、`config/`、`internal/`、`main.go` 等全部文件。

### 1.3 开启 Actions 写权限（关键步骤）

工作流需要自动提交 `dist/` 与 `config/discovered.yaml`，必须授予写权限：

1. 进入仓库 **Settings → Actions → General**。
2. **Workflow permissions** 选择 **Read and write permissions**。
3. 保存。

> 漏掉此步，工作流的"提交产物"步骤会因无权限而失败。

### 1.4 配置仓库变量（推荐）

1. 进入 **Settings → Environments and variables → Variables**。
2. 点击 **New repository variable**：
   - 名称：`TVBOX_JAR_BASE_URL`
   - 值：`https://你的域名/jar`（Cloudflare 部署完成后填写）
3. 保存。

用途：产物中爬虫站的 `spider` 地址自动重写为该地址下的本地托管 jar（防上游失效与篡改）。未配置时不重写。

### 1.5 手动触发首次运行（验证）

1. 进入仓库 **Actions** 页面，选择 **tvbox-aggregator** 工作流。
2. 点击 **Run workflow**（分支选 main），手动执行一次。
3. 观察日志各步骤：
   - ✅ 检出代码、设置 Go（依赖缓存）、构建成功
   - ✅ 运行聚合测速：输出加载源池、测速统计、发现通道日志
   - ✅ 提交产物：显示提交成功（首次运行自动创建初始提交；后续运行使用 `commit --amend` 仅保留最新快照）

### 1.6 验证提交结果

仓库文件列表应出现：

- `dist/tvbox.json`（单一订阅版，15 条源）
- `dist/status.json`（诊断文件）
- `config/discovered.yaml`（发现源池）

> 注意：当前 `config/sources.yaml` 中为示例源池，域名已失效，首轮 `tvbox.json` 可能为空或仅含保底源——补充真实采集站后次轮即正常填充。

### 1.7 调度说明

- **定时**：每日 UTC 0 点（北京时间早 8 点）自动运行，无需操作。
- **手动**：随时可在 Actions 页面手动触发。
- **改频率**：编辑 `.github/workflows/auto-update.yml` 的 cron 字段（最小粒度 1 小时），推送后生效。

---

## 2. Cloudflare 侧：CDN 分发

### 2.1 连接 Git 仓库

1. 登录 Cloudflare 控制台，进入 **Workers & Pages**。
2. 点击 **Create → Pages → Connect to Git**。
3. 授权 GitHub，选择 `tvbox-aggregator` 仓库。

### 2.2 构建配置

保持默认即可：

- **构建命令**：留空（纯静态仓库，无需构建）
- **输出目录**：`/`（根目录）
- **生产分支**：`main`

点击 **Deploy**，等待首次部署完成。

### 2.3 绑定自定义域名（国内可达性关键）

`*.pages.dev` 免费域名在国内可能被间歇性干扰，**务必绑定自定义域名**（Pages 免费支持，无额外费用）：

1. 进入 Pages 项目 → **Custom domains → Add custom domain**。
2. 输入域名（如 `tv.example.com`）。
3. 按提示配置 DNS 记录（二选一）：
   - **方式一（推荐）**：将域名的 NS 记录指向 Cloudflare，整个域名托管到 Cloudflare；
   - **方式二**：在现有 DNS 服务商处添加 **CNAME** 记录，值填 `<项目名>.pages.dev`。
4. 等待验证通过（通常几分钟，最长数小时）。

### 2.4 自动同步机制

- 每次 GitHub Actions 提交 `dist/` 后，Pages 在约 **10 秒**内自动同步至全球 CDN 节点。
- 无需任何手动操作，后续更新全自动。

### 2.5（可选）Worker 短链

若想将订阅地址缩短为 `https://你的域名/tv`：

1. **Workers & Pages → Create → Worker**，编写路由脚本将 `/tv` 重写/重定向到 `/dist/tvbox.json`。
2. 绑定自定义域名或路由规则。
3. 非必需，订阅直链 `/dist/tvbox.json` 已可直接使用。

---

## 3. jar 本地托管（使用爬虫站时）

1. 将确认可自由分发的 spider jar 放入仓库 `jar/` 目录（如 `jar/spider.jar`）。
2. `git add jar/ && git commit && git push`。
3. 确保仓库变量 `TVBOX_JAR_BASE_URL` 已配置为 `https://你的域名/jar`。
4. 产物中爬虫站的 `spider` 字段将自动重写为 `https://你的域名/jar/spider.jar`，防上游失效与篡改。

---

## 4. 部署后验证清单

| 检查项 | 方法 | 预期结果 |
| :--- | :--- | :--- |
| 工作流运行 | GitHub Actions 页面 | 每日自动运行，日志全绿，产物自动提交 |
| 订阅直链 | 浏览器访问 `https://你的域名/dist/tvbox.json` | 返回 15 条源的 JSON |
| 诊断文件 | 访问 `https://你的域名/dist/status.json` | 返回测速与探测明细 |
| 兜底直链 | 访问 `https://cdn.jsdelivr.net/gh/<用户>/<仓库>@main/dist/tvbox.json` | Pages 故障时的备用通道，可访问 |
| 客户端实测 | TVBox/影视仓粘贴订阅地址 | 线路列表正常加载，搜索可用 |
| 同步验证 | 客户端刷新订阅后对比线路列表 | 与最新 `tvbox.json` 一致 |

---

## 5. 常见问题排查

| 问题 | 排查方向 |
| :--- | :--- |
| 工作流"提交产物"失败 | 检查 Actions 写权限是否开启（Settings → Actions → General） |
| 工作流构建失败 | 检查 Go 版本与网络（依赖缓存是否命中）；重跑一次 |
| 发现通道未产出源 | 本地无 `GITHUB_TOKEN` 时 GitHub 通道自动跳过（CI 中自动注入，无需配置）；索引站通道检查 `discover.yaml` 的 `index_sites` |
| 产物未更新 | 查看提交步骤日志：产物无变化时会跳过提交（属正常）；有变化但未提交则检查权限 |
| 客户端刷新后仍是旧配置 | 客户端订阅管理中清除缓存再刷新；检查订阅直链响应头 `Cache-Control`（Pages 每次部署即更新边缘内容） |
| 自定义域名验证失败 | 核对 DNS 记录（CNAME 值、NS 记录）是否与 Pages 提示一致 |
| 国内访问慢或不通 | 确认已绑定自定义域名（`*.pages.dev` 可能被干扰）；检查 CF 区域与 DNS 生效状态 |

---

## 6. 日常维护

1. **巡检**：每月查看一次 `dist/status.json`，核对失效/海外封锁源。
2. **源池更新**：在 `config/sources.yaml` 追加可用采集站、更新 `config/whitelist.yaml`，下轮自动测速入榜。
3. **发现通道**：按 `config/discover.yaml` 维护索引站列表；稳定发现源可复制到 `sources.yaml` 提升为人工信任级。
4. **合规**：仓库公开时，切勿在配置或代码中硬编码个人隐私、付费 Token 或私有代理密码。
