package api

import _ "embed"

// specJSON 是 OpenAPI 3.1 契约原文。选用 JSON 而非 YAML 是为了用标准库即可校验，
// 不引入额外依赖；主流生成器同样支持 JSON。
//
//go:embed openapi.json
var specJSON []byte

// SpecJSON 返回契约原文。
func SpecJSON() []byte { return specJSON }
