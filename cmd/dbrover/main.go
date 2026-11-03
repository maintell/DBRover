// Command dbrover 是 DBRover 的 CLI 与 serve 入口。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/driver/redis"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/infra/tunnel"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// version 由 CI 通过 -ldflags "-X main.version=<tag>" 注入。
// 必须是 var：const 无法被链接器改写。
var version = "0.1.0-dev"

// errBlocked 表示操作被策略拒绝。退出码 2，便于脚本区分「拒绝」与「失败」。
var errBlocked = errors.New("操作被策略拒绝")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("dbrover %s\n", version)
	case "help", "-h", "--help":
		usage()
	case "health":
		err = cmdHealth(os.Args[2:])
	case "tables":
		err = cmdTables(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "tunnel":
		err = cmdTunnel(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}

	switch {
	case err == nil:
	case errors.Is(err, errBlocked):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dbrover —— 跨引擎数据库工作台

用法:
  dbrover version
  dbrover health -db <file> [-env ...] [-ro|-rw]
  dbrover tables -db <file> [-env ...] [-ro|-rw]
  dbrover exec   -db <file> (-sql "..." | -file <path>) [-env ...] [-ro|-rw] [-approve]
  dbrover serve  [-addr 127.0.0.1:7788] [-token <token>] [-allow-remote]
  dbrover tunnel -via user@hop1:22[,user@hop2:22] -to host:port (-password-stdin | -key <path>)

退出码:
  0  成功
  1  执行失败
  2  被策略拒绝（只读连接上的写操作、缺少审批等）
`)
}

// normalizeEngine 接受常见别名，避免用户记错写法。
func normalizeEngine(s string) model.Engine {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pg", "postgres", "postgresql":
		return model.EnginePostgres
	case "maria", "mariadb":
		return model.EngineMariaDB
	case "sqlite3":
		return model.EngineSQLite
	default:
		return model.Engine(strings.ToLower(strings.TrimSpace(s)))
	}
}

// connFlags 是各子命令共用的连接参数。
// SQLite 用 -db 指文件路径；网络引擎用 -host/-port/-user，-db 指库名。
type connFlags struct {
	engine string
	db     string
	// schema 与 db 是两件事：db 是库名，schema 留空则用引擎默认值
	// （PostgreSQL 为 public）。把库名当 schema 会让元数据查询全部落空。
	schema    string
	host      string
	port      int
	user      string
	passStdin bool
	passFile  string
	env       string
	ro        bool
	rw        bool
}

func (c *connFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.engine, "engine", "sqlite", "引擎: sqlite|mysql|postgres")
	fs.StringVar(&c.db, "db", "", "SQLite 数据库文件路径；MySQL/PostgreSQL 为库名")
	fs.StringVar(&c.schema, "schema", "", "schema（可选，PostgreSQL 默认 public；不要传库名）")
	fs.StringVar(&c.host, "host", "", "主机地址（网络引擎必填）")
	fs.IntVar(&c.port, "port", 0, "端口（默认按引擎取）")
	fs.StringVar(&c.user, "user", "", "登录用户名（网络引擎必填）")
	fs.BoolVar(&c.passStdin, "password-stdin", false, "从标准输入读取密码")
	fs.StringVar(&c.passFile, "password-file", "", "从文件读取密码（比管道更可靠，适合脚本）")
	fs.StringVar(&c.env, "env", "unknown", "环境: local|test|staging|production|unknown")
	fs.BoolVar(&c.ro, "ro", false, "强制只读（优先于环境默认值）")
	fs.BoolVar(&c.rw, "rw", false, "显式声明读写：生产/预发默认只读，需写操作时用本开关")
}

func (c *connFlags) open() (driver.Connector, model.Environment, model.AccessMode, error) {
	if c.ro && c.rw {
		return nil, "", "", errors.New("-ro 与 -rw 不能同时使用")
	}
	engine := normalizeEngine(c.engine)
	env := model.ParseEnvironment(c.env)

	// 默认跟随环境：生产/预发/未声明 → 只读；测试/本地 → 读写。
	// 需要写生产时必须显式 -rw，这是刻意抬高摩擦。
	mode := model.DefaultAccessMode(env)
	if c.ro {
		mode = model.AccessReadOnly
	}
	if c.rw {
		mode = model.AccessReadWrite
	}

	cfg := driver.ConnectConfig{
		Name:        "cli",
		Host:        c.host,
		Port:        c.port,
		Catalog:     c.db,
		Schema:      c.schema,
		User:        c.user,
		AccessMode:  mode,
		Environment: env,
	}

	switch engine {
	case model.EngineSQLite:
		if c.db == "" {
			return nil, "", "", errors.New("SQLite 需要 -db 指定文件路径")
		}
		cfg.Host = c.db
	default:
		if c.host == "" {
			return nil, "", "", fmt.Errorf("%s 需要 -host", engine)
		}
		if c.user == "" {
			return nil, "", "", fmt.Errorf("%s 需要 -user", engine)
		}
		if c.passFile != "" {
			b, err := os.ReadFile(c.passFile)
			if err != nil {
				return nil, "", "", fmt.Errorf("读取密码文件: %w", err)
			}
			cfg.Password = strings.TrimRight(string(b), "\r\n")
		} else if c.passStdin {
			pw, err := readSecretLine("密码: ")
			if err != nil {
				return nil, "", "", err
			}
			cfg.Password = string(pw)
		}
	}

	opened, err := openConnector(engine, cfg)
	if err != nil {
		return nil, "", "", err
	}
	// CLI 的 health / tables / exec 都建立在关系型语义上（表、行、SQL）。
	// 非关系型引擎（Redis）走 serve 的 /kv 端点，这里明确拒绝而不是给个空实现。
	conn, ok := opened.(driver.Connector)
	if !ok {
		return nil, "", "", fmt.Errorf(
			"CLI 的 health / tables / exec 只支持关系型引擎；%s 请用 `serve` 后的 /kv 端点", engine)
	}
	return conn, env, mode, nil
}

func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	var cf connFlags
	cf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, env, mode, err := cf.open()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	h, err := conn.Health(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("engine      %s\n", conn.Engine())
	fmt.Printf("version     %s\n", h.Version)
	fmt.Printf("environment %s\n", env)
	fmt.Printf("access      %s\n", mode)
	fmt.Printf("read_only   %v\n", h.ReadOnly)
	fmt.Printf("host        %s\n", h.Identity.Hostname)
	fmt.Printf("fingerprint %s\n", h.Identity.Fingerprint)
	for _, w := range h.Warnings {
		fmt.Printf("warning     %s\n", w)
	}
	return nil
}

func cmdTables(args []string) error {
	fs := flag.NewFlagSet("tables", flag.ExitOnError)
	var cf connFlags
	cf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, _, _, err := cf.open()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	objs, err := conn.Meta().Objects(context.Background(), "", "", model.KindTable, model.KindView)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		fmt.Println("(没有表)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME")
	for _, o := range objs {
		fmt.Fprintf(tw, "%s\t%s\n", o.Kind, o.Name)
	}
	return tw.Flush()
}

func cmdExec(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	var cf connFlags
	cf.bind(fs)
	sqlText := fs.String("sql", "", "要执行的 SQL")
	sqlFile := fs.String("file", "", "从文件读取 SQL")
	approve := fs.Bool("approve", false, "已获得人工确认（F2 及以上需要）")
	maxRows := fs.Int("max-rows", 50, "单条查询最大显示行数")
	if err := fs.Parse(args); err != nil {
		return err
	}

	text := *sqlText
	if *sqlFile != "" {
		b, err := os.ReadFile(*sqlFile)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("需要 -sql 或 -file")
	}

	conn, env, mode, err := cf.open()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	audit := &service.SliceAudit{}
	ex := service.NewExecutor(conn, env, mode, audit)

	out, err := ex.Execute(context.Background(), service.Request{
		SQL:      text,
		Approval: *approve,
		MaxRows:  *maxRows,
	})
	if err != nil {
		return err
	}

	if out.Blocked {
		fmt.Fprintf(os.Stderr, "已拒绝: %s\n", out.Reason)
		fmt.Fprintf(os.Stderr, "风险=%s 打扰=%s 环境=%s\n", out.Risk, out.Friction, env)
		if out.Friction >= 2 && out.Friction < 5 && !*approve {
			fmt.Fprintln(os.Stderr, "提示: 确认无误后加 -approve 重新执行")
		}
		return errBlocked
	}

	failed := false
	for _, r := range out.Results {
		fmt.Printf("-- #%d %s 风险=%s 耗时=%dms\n", r.Index, firstLine(r.SQL), r.Grade.Level, r.DurationMS)
		if r.Err != "" {
			fmt.Printf("错误: %s\n", r.Err)
			failed = true
			continue
		}
		if len(r.Columns) > 0 {
			printTable(r.Columns, r.Rows)
		} else {
			fmt.Printf("影响 %d 行\n", r.Affected)
		}
	}

	fmt.Printf("执行完成: 风险=%s 打扰=%s 耗时=%s 审计=%d 条\n",
		out.Risk, out.Friction, out.Duration.Round(time.Millisecond), len(audit.Events))

	// 语句失败必须体现为退出码：脚本靠退出码判断，不能只看 stdout。
	if failed {
		code := "SQL_EXEC_FAILED"
		if out.Err != nil {
			code = out.Err.Code
		}
		return fmt.Errorf("执行失败（%s）", code)
	}
	return nil
}

// openConnector 是按引擎分发的连接器工厂。api 层只接受函数，不认识具体驱动。
func openConnector(engine model.Engine, cfg driver.ConnectConfig) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch engine {
	case model.EngineSQLite:
		c := sqlite.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EngineMySQL, model.EngineMariaDB:
		c := mysql.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EnginePostgres:
		c := pg.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EngineRedis:
		// 非关系型：实现的是 driver.KVConnector，不是 driver.Connector（ADR-0007）。
		c := redis.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("暂不支持的引擎: %s", engine)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "监听地址（默认仅回环）")
	token := fs.String("token", "", "访问令牌（默认随机生成）")
	allowRemote := fs.Bool("allow-remote", false, "允许监听非回环地址")
	auditPath := fs.String("audit", "", "审计日志文件（JSONL 追加写；留空则只保留内存）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	audit, err := service.NewAuditLog(*auditPath, 0)
	if err != nil {
		return err
	}
	defer func() { _ = audit.Close() }()

	s := api.New(*token, openConnector, audit)
	defer s.Close()

	fmt.Fprintf(os.Stderr, "dbrover 监听 http://%s\n", *addr)
	fmt.Fprintf(os.Stderr, "界面: http://%s/#t=%s\n", *addr, s.Token())
	fmt.Fprintf(os.Stderr, "API : curl -H 'X-DBRover-Token: %s' http://%s/api/v1/connections\n", s.Token(), *addr)
	if p := audit.Path(); p != "" {
		fmt.Fprintf(os.Stderr, "审计: %s\n", p)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return s.Serve(ctx, *addr, *allowRemote)
}

// parseHops 解析 -via 参数：user@host:port[,user@host:port]。
func parseHops(spec string, password, privateKey []byte) ([]tunnel.Hop, error) {
	parts := strings.Split(spec, ",")
	hops := make([]tunnel.Hop, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		at := strings.LastIndex(p, "@")
		if at <= 0 || at == len(p)-1 {
			return nil, fmt.Errorf("跳板格式应为 user@host:port，实际 %q", p)
		}
		hop := tunnel.Hop{Addr: p[at+1:], User: p[:at]}
		if len(privateKey) > 0 {
			hop.AuthKind = tunnel.AuthPrivateKey
			hop.PrivateKey = privateKey
		} else {
			hop.AuthKind = tunnel.AuthPassword
			hop.Password = password
		}
		hops = append(hops, hop)
	}
	if len(hops) == 0 {
		return nil, errors.New("-via 至少要有一个跳板")
	}
	return hops, nil
}

// readSecretLine 从标准输入读一行，避免密码出现在命令行与进程列表里。
func readSecretLine(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return nil, fmt.Errorf("读取标准输入失败: %w", err)
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

func cmdTunnel(args []string) error {
	fs := flag.NewFlagSet("tunnel", flag.ExitOnError)
	via := fs.String("via", "", "跳板，格式 user@host:port，多跳用逗号分隔")
	to := fs.String("to", "", "目标 host:port")
	passwordStdin := fs.Bool("password-stdin", false, "从标准输入读取跳板密码")
	keyPath := fs.String("key", "", "私钥文件路径")
	knownHosts := fs.String("known-hosts", defaultKnownHostsPath(), "主机密钥信任库（JSON）")
	acceptHostKey := fs.Bool("accept-host-key", false, "首次连接时接受并记录跳板主机密钥")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *via == "" || *to == "" {
		return errors.New("-via 与 -to 必填")
	}

	var password, privateKey []byte
	if *keyPath != "" {
		b, err := os.ReadFile(*keyPath)
		if err != nil {
			return fmt.Errorf("读取私钥: %w", err)
		}
		privateKey = b
	} else if *passwordStdin {
		pw, err := readSecretLine("跳板密码: ")
		if err != nil {
			return err
		}
		password = pw
	} else {
		return errors.New("需要 -password-stdin 或 -key")
	}

	hops, err := parseHops(*via, password, privateKey)
	if err != nil {
		return err
	}

	store, err := tunnel.NewFileHostKeys(*knownHosts)
	if err != nil {
		return err
	}
	// 默认拒绝未知主机：不静默信任首连。
	if *acceptHostKey {
		store.SetOnFirstUse(func(addr, fingerprint string) bool {
			fmt.Fprintf(os.Stderr, "接受主机密钥 %s %s\n", addr, fingerprint)
			return true
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	tr, err := tunnel.Dial(ctx, tunnel.Config{Hops: hops, HostKeys: store}, *to)
	if err != nil {
		return err
	}
	defer func() { _ = tr.Close() }()

	fmt.Printf("隧道已建立：%d 跳，耗时 %s\n", tr.Hops(), time.Since(started).Round(time.Millisecond))
	for _, h := range hops {
		if fp, ok := store.Known(h.Addr); ok {
			fmt.Printf("  %-28s %s\n", h.Addr, fp)
		}
	}
	fmt.Printf("信任库 %s\n", store.Path())
	fmt.Printf("目标 %s 可连通\n", *to)
	return nil
}

// defaultKnownHostsPath 返回默认信任库位置。
func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "dbrover_known_hosts.json"
	}
	return filepath.Join(home, ".dbrover", "known_hosts.json")
}

func printTable(cols []model.Column, rows [][]any) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	fmt.Fprintln(tw, strings.Join(names, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = cellString(v)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
	fmt.Printf("(%d 行)\n", len(rows))
}

func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("0x%X", x)
	case string:
		if x == "" {
			return "''"
		}
		return x
	default:
		return fmt.Sprint(x)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
