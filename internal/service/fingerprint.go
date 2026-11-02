package service

import (
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

// Fingerprint 返回一条 SQL 的稳定指纹（归一化后取 SHA-256）。
//
// 审批必须绑定这个值：审批的是「这段内容」，不是「这个请求」。
// 调用方拿到指纹后可以签发审批凭证，执行前再算一次比对。
func Fingerprint(engine model.Engine, sqlText string) (string, error) {
	return dialect.For(engine).Fingerprint(sqlText)
}
