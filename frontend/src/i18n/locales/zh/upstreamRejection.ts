export default {
  "upstreamRejection": {
    "eligibleOnly": "仅Grok余额计费分组可启用；变更平台或计费类型前请先关闭。",
    "customerPrice": "¥{price}/次",
    "title": "内容限制处理费",
    "enabled": "启用内容限制计费",
    "price": "单次费用（元）",
    "hint": "请求因内容限制被拒绝时，按 ¥{price}/次计费，不另收用量费，不受倍率影响。",
    "invalidPrice": "请输入非负金额，最多8位小数；启用时必须大于0。",
    "accountTitle": "内容限制识别",
    "accountEnabled": "启用内容限制识别",
    "accountHint": "识别内容限制响应，命中后停止重试。计费开关和单次费用在分组设置。",
    "cost": "单次成本（USD）",
    "currency": "成本以USD记录，与客户费用分别核算。",
    "filterHint": "仅导入不含 / 或 * 的Grok模型名称。模型价格与可用能力需单独确认。",
    "invalidModel": "内容限制识别仅支持不含 / 或 * 的Grok模型名称，请调整白名单与映射。",
    "groupCompatibility": "请使用独立分组，且所有账号均须启用内容限制识别。当前支持HTTP/SSE，不支持WebSocket或设有Grok平台日、周、月限额的用户；不满足条件时将拒绝请求。"
  }
}
