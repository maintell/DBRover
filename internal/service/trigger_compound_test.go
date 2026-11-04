package service_test

import (
	"context"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestCompoundTriggerThroughSplitter 是真机验证：复合触发器体（BEGIN … END、
// 内含 IF … END IF）经过切分器后仍然是一条完整语句，能被引擎接受，并且体内
// 逻辑真的生效。
//
// 切分器本身有单测，但「单测说切对了」和「引擎说这条语句合法」是两件事：
// 少一个分号或多了半个 END，只有真库会报错。
//
// 只跑 MySQL：PG 的触发器要先建触发器函数（两个对象），那条路径由 drivertest
// 的 TriggerListing 覆盖；这里专门钉住 MySQL 的复合体。
func TestCompoundTriggerThroughSplitter(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		if tgt.name != "mysql" {
			continue
		}
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_trg_compound",
				"CREATE TABLE it_trg_compound (id INT PRIMARY KEY, v INT NOT NULL DEFAULT 0, note VARCHAR(32))",
			}, nil)

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			script := `CREATE TRIGGER trg_compound BEFORE INSERT ON it_trg_compound
FOR EACH ROW
BEGIN
  IF NEW.v > 0 THEN
    SET NEW.note = 'positive';
  ELSE
    SET NEW.note = 'non-positive';
  END IF;
END;
SELECT 1 AS ok;`

			out, err := ex.Execute(ctx, service.Request{SQL: script, Approval: true, Actor: "human:test"})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if out.Blocked {
				t.Fatalf("被拦下了：%s", out.Reason)
			}
			if out.Err != nil {
				t.Fatalf("执行报错：%s", out.Err.Message)
			}
			if len(out.Results) != 2 {
				t.Fatalf("应切成 2 条语句（触发器 + SELECT），实际 %d 条", len(out.Results))
			}

			objs, err := conn.Meta().Objects(ctx, "", "", model.KindTrigger)
			if err != nil {
				t.Fatalf("Objects(KindTrigger): %v", err)
			}
			found := false
			for _, o := range objs {
				if o.Name == "trg_compound" {
					found = true
				}
			}
			if !found {
				t.Fatalf("触发器没建出来：%+v", objs)
			}

			// 体内逻辑真的生效：>0 记 positive，否则记 non-positive。
			for _, c := range []struct {
				id, v int
				want  string
			}{{1, 5, "positive"}, {2, 0, "non-positive"}} {
				if _, err := ex.Execute(ctx, service.Request{
					SQL:  "INSERT INTO it_trg_compound (id, v) VALUES (?, ?)",
					Args: []any{c.id, c.v}, Approval: true, Actor: "human:test",
				}); err != nil {
					t.Fatalf("插入 id=%d: %v", c.id, err)
				}
			}
			res, err := ex.Execute(ctx, service.Request{
				SQL: "SELECT note FROM it_trg_compound ORDER BY id", Actor: "human:test",
			})
			if err != nil || res.Err != nil {
				t.Fatalf("查询: err=%v out=%+v", err, res)
			}
			if len(res.Results) != 1 || len(res.Results[0].Rows) != 2 {
				t.Fatalf("结果异常：%+v", res.Results)
			}
			for i, want := range []string{"positive", "non-positive"} {
				got := res.Results[0].Rows[i][0]
				if got != want {
					t.Errorf("第 %d 行 note = %v，期望 %s（触发器体没生效）", i+1, got, want)
				}
			}
		})
	}
}
