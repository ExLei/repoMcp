// repoMcp —— 给 LangBot 用的仓库源码检索 MCP 服务。
//
// 让 IM 机器人里的 LLM 能够检索白名单仓库的源码、符号与提交历史，
// 并带着「路径:行号 + 钉住 commit 的链接」回答问题，答案可被人工核验。
//
// 传输：无状态 Streamable HTTP，端点 POST /mcp（Bearer 鉴权），
// 在 LangBot 中按 mode=http（或 remote 自动探测）接入即可。
//
// 检索：本地 clone + 内存倒排索引（BM25）+ 正则符号表。不依赖 embedding、
// 不依赖外部检索服务，全部标准库实现，可 CGO_ENABLED=0 交叉编译为单二进制。
//
// 命令行：
//
//	repomcp -config config.json
//
// 环境变量覆盖：REPOMCP_CONFIG / REPOMCP_LISTEN / REPOMCP_TOKEN / REPOMCP_DATA
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server 持有全部运行时依赖。索引、仓库与 issue 层各自保证并发安全，Server 本身无状态。
type Server struct {
	cfg     *Config
	store   *Store
	index   *Index
	gh      *GitHub
	limiter *issueRateLimiter
	// mediaLimiter 限制每仓每小时媒体上传数（独立于 issue 创建限额）。
	mediaLimiter       *issueRateLimiter
	attachmentUploader attachmentUploader
	attachmentStatus   *attachmentStatusCache

	// 外部管理员名单（AstrBot admins_id）缓存：mtime 变化时重读。
	adminsMu    sync.Mutex
	adminsCache []string
	adminsMtime time.Time
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[repomcp] ")

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("配置错误：%v", err)
	}
	repos, err := cfg.BuildRepos()
	if err != nil {
		log.Fatalf("配置错误：%v", err)
	}
	if cfg.Token == "" {
		log.Printf("警告：未设置 token，MCP 端点无鉴权。仅应在受信网络或 127.0.0.1 上这样运行。")
	}

	nativeUploader, nativeStatus, nativeErr := newNativeAttachmentUploader(cfg)
	if nativeErr != nil {
		log.Printf("警告：GitHub 原生附件上传不可用：%v", nativeErr)
	} else if nativeStatus.Authenticated {
		log.Printf("GitHub 原生附件账号已认证：%s", nativeStatus.Account)
	}
	srv := &Server{
		cfg:                cfg,
		store:              NewStore(repos),
		index:              NewIndex(),
		gh:                 NewGitHub(cfg.GitHubAPIBase, cfg.ghTimeout),
		limiter:            newIssueRateLimiter(cfg.issueLimit),
		mediaLimiter:       newIssueRateLimiter(cfg.mediaUploadLimit),
		attachmentUploader: nativeUploader,
		attachmentStatus:   newAttachmentStatusCache(nativeStatus),
	}
	logIssueSetup(repos, cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 工具调用可能触发 git 子进程，写超时需宽于 gitTimeout。
		WriteTimeout: cfg.gitTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.mediaSweepLoop(ctx)

	go srv.syncLoop(ctx)

	go func() {
		log.Printf("监听 %s，仓库 %d 个，端点 POST /mcp", cfg.Listen, len(repos))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出：%v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭超时：%v", err)
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// logIssueSetup 把 issue 能力的实际生效情况打到日志。
// 这类「配置写了但没生效」的问题排查成本极高，启动时讲清楚最省事。
func logIssueSetup(repos []*Repo, cfg *Config) {
	var read, write []string
	for _, r := range repos {
		if !r.IssueRead {
			continue
		}
		if r.IssueWrite {
			write = append(write, r.Name+"="+r.Slug)
			continue
		}
		read = append(read, r.Name+"="+r.Slug)
	}
	if len(read) == 0 && len(write) == 0 {
		log.Printf("issue 工具未启用（没有仓库配置 issues 段）")
		return
	}
	if len(read) > 0 {
		log.Printf("issue 只读：%s", strings.Join(read, " "))
	}
	if len(write) > 0 {
		limit := "不限"
		if cfg.issueLimit > 0 {
			limit = strconv.Itoa(cfg.issueLimit) + " 个/小时"
		}
		log.Printf("issue 可写：%s（创建上限 %s）", strings.Join(write, " "), limit)
	}
	for _, r := range repos {
		if r.IssueRead && r.GHToken == "" {
			log.Printf("警告：仓库 %s 的 issue 未配置令牌，只能读公开仓且限流严格（60 次/小时）", r.Name)
		}
	}
}

// syncLoop 启动时立即同步一次，之后按配置周期重复。
// 服务在首次索引完成前即可接受请求，工具会返回「索引进行中」而非空结果。
func (s *Server) syncLoop(ctx context.Context) {
	s.syncOnce(ctx)
	if s.cfg.syncInterval <= 0 {
		log.Printf("已关闭定时同步（syncInterval=0）")
		return
	}
	t := time.NewTicker(s.cfg.syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncOnce(ctx)
		}
	}
}

// syncOnce 拉取远端并重建索引。单仓失败被隔离：其余仓库照常索引。
func (s *Server) syncOnce(ctx context.Context) {
	repos := s.store.Repos()
	// 每仓一份 git 超时预算，外加一份余量，避免慢仓拖垮整轮同步。
	deadline := s.cfg.gitTimeout * time.Duration(len(repos)+1)
	syncCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	started := time.Now()
	if err := s.store.Sync(syncCtx); err != nil {
		log.Printf("同步存在失败：%v", err)
	}

	for _, r := range repos {
		if syncCtx.Err() != nil {
			log.Printf("同步超时，剩余仓库本轮跳过")
			return
		}
		if !r.HasCode {
			continue // 反馈仓库无源码，不加载不索引
		}
		t0 := time.Now()
		files, err := s.store.Load(r)
		if err != nil {
			log.Printf("加载 %s 失败：%v", r.Name, err)
			continue
		}
		s.index.Replace(r.Name, files)
		st := s.index.Stats()[r.Name]
		log.Printf("已索引 %s @%s：%d 文件 / %d 行 / %d 符号（%s）",
			r.Name, shortSHA(s.store.Head(r.Name)), st.Files, st.Lines, st.Symbols,
			time.Since(t0).Round(time.Millisecond))
	}
	log.Printf("本轮同步完成，耗时 %s", time.Since(started).Round(time.Millisecond))
}
