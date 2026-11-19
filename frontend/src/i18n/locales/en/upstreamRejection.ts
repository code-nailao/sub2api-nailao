export default {
  "upstreamRejection": {
    "eligibleOnly": "Available only for Grok balance-billed groups. Disable this setting before changing the platform or billing type.",
    "customerPrice": "¥{price} per request",
    "title": "Content restriction processing fee",
    "enabled": "Enable content restriction billing",
    "price": "Fee per request (CNY)",
    "hint": "Requests rejected due to content restrictions are charged ¥{price} per request, with no additional usage charge or multipliers.",
    "invalidPrice": "Enter a non-negative amount with up to 8 decimal places. An enabled fee must be greater than zero.",
    "accountTitle": "Content restriction detection",
    "accountEnabled": "Enable content restriction detection",
    "accountHint": "Detects content restriction responses and stops retries when matched. Configure billing and the fee per request in group settings.",
    "cost": "Cost per request (USD)",
    "currency": "Costs are recorded in USD, separately from customer charges.",
    "filterHint": "Import only Grok model names without / or *. Verify model prices and capabilities separately.",
    "invalidModel": "Content restriction detection requires Grok model names without / or *. Update the allowlist and mappings.",
    "groupCompatibility": "Use a dedicated group with content restriction detection enabled on every account. HTTP/SSE is supported. WebSocket and users with Grok daily, weekly or monthly platform limits are not supported; requests that do not meet these conditions are rejected."
  }
}
