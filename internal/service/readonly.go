package service

import (
	"context"
	"errors"

	"github.com/maintell/DBRover/internal/driver/dialect"
)

// ErrReadOnlyRequired 表示调用方想执行一条会写数据的语句，但它走的是只读入口。
var ErrReadOnlyRequired = errors.New("这个入口只接受只读查询")

// ReadOnlyQuery 执行一条**只读**查询。
//
// 为什么要单独一个方法，而不是让调用方先用 Analyze 判一下再调 Execute：
// 这类入口的主要调用方是 Agent，而 Agent 的输入可以被数据内容操纵。
// 把「这是不是只读语句」的判断留在调用方，等于把边界交给最不可信的一方。
// 这里由服务端自己分类：任何会写、或者认不出来的语句一律拒绝，并指路变更集。
func (e *Executor) ReadOnlyQuery(ctx context.Context, req Request) (*Outcome, error) {
	an, err := e.dialect.Analyze(req.SQL)
	if err != nil {
		return nil, err
	}
	if an.Writes || an.Class != dialect.ClassQuery {
		return nil, &ReadOnlyViolation{Verb: an.Verb, Class: an.Class}
	}
	return e.Execute(ctx, req)
}

// ReadOnlyViolation 说明为什么一条语句不能走只读入口。
//
// 单独一个类型是为了让上层能给出可行动的提示（「改用变更预案」），
// 而不是把一句 errors.New 直接丢给用户。
type ReadOnlyViolation struct {
	Verb  string
	Class dialect.Class
}

func (v *ReadOnlyViolation) Error() string {
	return "不是只读查询（" + string(v.Class) + "：" + v.Verb + "），只读入口拒绝执行"
}

// IsReadOnlyViolation 判断错误是不是「这不是只读查询」。
func IsReadOnlyViolation(err error) bool {
	var v *ReadOnlyViolation
	return errors.As(err, &v)
}

// Explain 渲染并执行一条执行计划。
//
// 只给计划、不执行：Agent 想在动手前看看代价，这个需求真实存在，
// 但「EXPLAIN ANALYZE」会真的跑一遍，所以这里固定用不带 ANALYZE 的形式。
func (e *Executor) Explain(ctx context.Context, sqlText string) (*Outcome, error) {
	wrapped, supported := e.dialect.ExplainSQL(sqlText)
	if !supported {
		return nil, ErrExplainUnsupported
	}
	return e.Execute(ctx, Request{SQL: wrapped, MaxRows: 200})
}

// ErrExplainUnsupported 表示该引擎没有能用单条语句表达的执行计划。
var ErrExplainUnsupported = errors.New("这个引擎不支持用单条语句取执行计划")
