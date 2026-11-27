# Sources Pricing Feed

独立的只读价格适配器。读取现有公开模型广场 `GET /api/v1/model-plaza`，输出 Provider Pricing 1.1 的 `GET /api/provider/pricing`。不依赖主站内部包或数据库，不持有用户、管理员或模型密钥，不调用生成接口。

## 配置与口径

复制 `config.example.json` 到私有配置目录，替换全部占位值。`quotes` 是显式白名单：`group_id` 定位主站分组，`group_name` 是外部系统已经登记的稳定标识，不随主站显示名称自动更改。每个组合必须在公开模型广场中存在；主站的模型登记和匿名开关通过已有管理后台设置。

`cny_per_credit` 表示一单位站内余额对应的人民币，以实际充值规则为准，不根据界面的货币符号猜测汇率。换算公式为：模型广场单 token 基础价 × 分组倍率 × 1,000,000 × `cny_per_credit`。使用有界的精确十进制运算，舍入到小数点后十二位；分组倍率只乘一次，源数据已有的分组价卡不再重算。

首版只输出 GPT、Codex、Claude 文本模型的 token 计费基础上下文档。其他模型族可能有模型广场未暴露的隐式分时规则，因此在配置阶段拒绝。阶梯模型会附带长上下文说明；缓存缺失项输出 `null`，明确配置的零保留为零，必需的输入价必须大于零。图片、音频、实时、按次、按秒、分时模型和高峰倍率分组不在此版本的输出范围；含图片输出价或非 1 思考等级倍率的价卡同样拒绝。个人专属倍率和私密分组被拒绝，避免把用户私价误发布为公开价。

输出包含 `schema_version`、`success`、`message`、`data`；`data` 包含站点名称、域名、固定币种 `CNY`、单位 `per_1m_tokens`、带时区的 `updated_at` 及模型列表。缓存字段分别映射为 `cache_input_price`、`cache_create_price`、`cache_create_price_1h`。

## 运行

```sh
go test -race ./...
go vet ./...
go build -o /YOUR_BUILD_DIR/pricing-feed ./cmd/pricing-feed
/YOUR_BUILD_DIR/pricing-feed -config /YOUR_PRIVATE_CONFIG/config.json -check-config
/YOUR_BUILD_DIR/pricing-feed -config /YOUR_PRIVATE_CONFIG/config.json -data-dir /YOUR_STATE_DIR -listen YOUR_LISTEN_ADDRESS
docker build -t YOUR_PRICING_IMAGE .
```

容器使用 UID/GID `10001`，只读挂载配置，并给它一个可写持久状态目录。`Dockerfile` 自带静态二进制和 CA 证书，工作端口为容器内部 `8080`。只将容器映射到宿主回环地址；Nginx 使用 `location = /api/provider/pricing` 精确转发，不接管其他主站路径。公网地址和部署记录放在私有运维仓库。

服务启动即同步，此后每五分钟同步一次。公共请求仅读快照，不触发上游抓取。状态目录通过操作系统文件锁保证单个写进程；快照写临时文件、fsync、原子替换后才对外发布，重启可恢复。配置指纹变化会使旧快照失效。

- `/healthz`：进程存活。
- `/readyz`：持有不超过三十分钟的有效报价。
- `/api/provider/pricing`：公开 JSON。冷启动无报价或报价过期时返回 HTTP 503 和 `success:false`，不输出空列表或零价格冒充成功。

抓取失败保留上次成功报价和原始时间戳；超过三十分钟即拒绝继续提供旧价。签名校验为外部协议的可选功能，本服务按公开价格源运行，不需要签名；自测工具携带的签名头不会影响公开读取。

上线先验证匿名源数据、换算和缓存行为，再通过外部抓取测试页验证，最后提交价格接口地址及分组对应表。接口开发完成、容器部署完成和外部系统启用同步是三个独立状态。
