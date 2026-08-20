// media.go：issue 媒体接收、服务器持久化、公开只读访问与孤儿清扫。
//
// create_issue / update_issue 的 images 参数（本地路径或 URL 列表）由服务端
// 下载 → 类型/大小校验 → 原子写入 MediaStoreDir → 渲染 MediaPublicBaseURL
// 链接进正文。持久目录或公开 URL 未配置时工具不暴露 images 参数。
//
// 安全边界（对抗审查产物，勿弱化）：
//   - URL 下载：host 必须命中 ImageDownloadHosts 白名单（默认 qpic.cn/qq.com），
//     解析 IP 必须全部为公网地址（拒绝回环/私网/链路本地/CGNAT/元数据段，
//     ImageDownloadAllowPrivate 显式打开才放行内网图源），DialContext 钉住
//     校验过的 IP、重定向逐跳重校验——三关共同封死 SSRF 与 DNS 重绑定，
//     Cookie 只发给白名单域名（凭据不外泄）。
//   - 本地路径：必须位于 MediaSourceDir 内；MediaSourcePrefix 可将调用方可见的
//     绝对路径映射到该目录。未配置源目录则拒绝本地路径（模型可被注入诱导，
//     不能让服务端替它读任意文件）。错误信息不回显路径与 errno（杜绝目录存在性 oracle）。
//   - 保存额度：每仓每小时 mediaUploadLimit 项（默认 20），独立于 issue 创建限额。
//
// 失败语义：媒体是正文的辅助信息，单个附件失败不阻断 issue 创建——失败项
// 以告警形式出现在工具返回里，由模型如实转告用户（视频过大按约定提示
// 「把视频直接传给开发者」）。
//
// 临时文件：URL 下载走 <MediaTempDir> 流式落盘，保存成功/失败都立即删除；
// 进程崩溃遗留的下载文件由 mediaSweepLoop 按 mediaMaxAge 兜底清扫。
//
// 孤儿媒体：持久保存成功但 issue 创建/更新失败（或进程崩溃）时，文件留在
// MediaStoreDir。mediaStoreSweepLoop 每天清扫：文件名符合本服务命名模式、
// 时间戳超过 mediaOrphanGrace、且随机 hex token 未被任何 issue 仓的 search
// 索引命中，才删除。引用核验失败时保留。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// mediaMaxBytes 是单个附件体积上限，避免耗尽服务器磁盘与请求预算。
	mediaMaxBytes = 100 << 20
	// mediaMaxCount 是单次调用可带的附件数上限。
	mediaMaxCount = 10
	// mediaMaxAge 是孤儿临时文件的兜底保留时长。
	mediaMaxAge = 24 * time.Hour
	// mediaSweepInterval 是孤儿临时文件的清扫周期。
	mediaSweepInterval = time.Hour
	// mediaMaxRedirects 是下载允许的最大重定向跳数。
	mediaMaxRedirects = 3
	// mediaOrphanGrace 是持久媒体的孤儿宽限期。
	mediaOrphanGrace = 7 * 24 * time.Hour
	// mediaStoreSweepInterval 是持久媒体孤儿清扫周期。
	mediaStoreSweepInterval = 24 * time.Hour

	mediaKindImage = "image"
	mediaKindVideo = "video"
)

// reMediaName 匹配服务端生成的媒体文件名：UTC 时间戳 + 12 位随机 hex + 扩展名。
// 不匹配的文件一律不公开、不清扫（fail-safe：宁可留着，绝不误删）。
var reMediaName = regexp.MustCompile(`^(\d{8}-\d{6})-([0-9a-f]{12})\.(png|jpg|gif|webp|mp4|mov)$`)

// mediaResult 是单次媒体处理的产物：md 为渲染进正文的 Markdown（可为空），
// imageCount / videoCount 为成功附带的分类计数（spec §10 概览粒度），
// warnings 为逐项失败原因（非致命）。
type mediaResult struct {
	md         string
	imageCount int
	videoCount int
	warnings   []string
}

// processMedia 下载、校验并持久保存 images 列表，返回可渲染进正文的 Markdown。
// 单项失败不阻断整体：失败原因进 warnings。保存受每仓每小时额度限制。
func (s *Server) processMedia(ctx context.Context, repoKey string, list []string) mediaResult {
	var res mediaResult
	if !s.cfg.mediaEnabled() || len(list) == 0 {
		return res
	}
	if len(list) > mediaMaxCount {
		res.warnings = append(res.warnings,
			fmt.Sprintf("附件超过 %d 个，仅处理前 %d 个", mediaMaxCount, mediaMaxCount))
		list = list[:mediaMaxCount]
	}

	var b strings.Builder
	for i, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		path, done, err := s.stageMedia(ctx, item)
		if err != nil {
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 下载失败：%v", i+1, err))
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			done()
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		kind, ext, err := sniffMedia(head[:n])
		if err != nil {
			_ = f.Close()
			done()
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d %v", i+1, err))
			continue
		}
		st, err := f.Stat()
		if err != nil {
			// Stat 失败不得绕过大小校验：按附件失败处理。
			_ = f.Close()
			done()
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		if st.Size() > mediaMaxBytes {
			_ = f.Close()
			done()
			if kind == mediaKindVideo {
				res.warnings = append(res.warnings,
					fmt.Sprintf("附件 %d 视频超过 100MB 无法随 issue 提交，请把视频直接传给开发者", i+1))
			} else {
				res.warnings = append(res.warnings,
					fmt.Sprintf("附件 %d 图片超过 100MB 无法随 issue 提交", i+1))
			}
			continue
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			done()
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		// 额度在真正持久保存前逐项扣除：校验/下载失败的项不占额度，
		// 保存失败也照扣（宁可少传，不能让失败重试变成刷额度）。
		if s.mediaLimiter != nil {
			if ok, wait := s.mediaLimiter.take(repoKey); !ok {
				_ = f.Close()
				done()
				res.warnings = append(res.warnings, fmt.Sprintf(
					"媒体保存达到每小时上限（%d 项），约 %d 分钟后可再试",
					s.cfg.mediaUploadLimit, int(wait.Minutes())+1))
				break
			}
		}
		name := mediaFileName(ext)
		publicURL, err := s.storeMedia(name, f)
		_ = f.Close()
		done()
		if err != nil {
			log.Printf("保存 issue 附件失败：%v", err)
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 保存失败（服务器存储不可用）", i+1))
			continue
		}
		if kind == mediaKindVideo {
			b.WriteString(fmt.Sprintf("[视频（浏览器不内嵌播放，点击下载）](%s)\n", publicURL))
			res.videoCount++
		} else {
			b.WriteString(fmt.Sprintf("![截图](%s)\n", publicURL))
			res.imageCount++
		}
	}
	res.md = strings.TrimSpace(b.String())
	return res
}

// storeMedia 把已校验媒体原子写入持久目录，并返回公开 URL。临时文件与最终文件
// 位于同一目录，rename 不会暴露半写文件。
func (s *Server) storeMedia(name string, src io.Reader) (publicURL string, err error) {
	tmp, err := os.CreateTemp(s.cfg.MediaStoreDir, ".media-*")
	if err != nil {
		return "", fmt.Errorf("创建持久媒体临时文件：%w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = io.Copy(tmp, src); err != nil {
		return "", fmt.Errorf("写入持久媒体：%w", err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return "", fmt.Errorf("设置持久媒体权限：%w", err)
	}
	if err = tmp.Sync(); err != nil {
		return "", fmt.Errorf("同步持久媒体：%w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("关闭持久媒体：%w", err)
	}
	finalPath := filepath.Join(s.cfg.MediaStoreDir, name)
	if err = os.Rename(tmpName, finalPath); err != nil {
		return "", fmt.Errorf("发布持久媒体：%w", err)
	}
	dir, openErr := os.Open(s.cfg.MediaStoreDir)
	if openErr != nil {
		return "", fmt.Errorf("打开持久媒体目录：%w", openErr)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return "", fmt.Errorf("同步持久媒体目录：%w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("关闭持久媒体目录：%w", closeErr)
	}
	return s.cfg.MediaPublicBaseURL + "/" + name, nil
}

// handlePublicMedia 只公开由本服务生成的扁平媒体文件名；不提供目录列表，
// 不接受任意相对路径。MCP Bearer 鉴权不适用于这些供 GitHub issue 渲染的 URL。
func (s *Server) handlePublicMedia(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.mediaEnabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	prefix := s.cfg.mediaPublicPath + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if !reMediaName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.cfg.MediaStoreDir, name)
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("读取公开 issue 媒体失败：%v", err)
		}
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// stageMedia 把单个附件落到本地文件：http(s) URL 逐跳校验下载到媒体临时目录，
// 本地路径经 confineLocalPath 校验后直接复用。done 负责清理下载的临时文件
// （本地路径场景是空操作，绝不删用户磁盘上的原文件）。
func (s *Server) stageMedia(ctx context.Context, item string) (path string, done func(), err error) {
	done = func() {}
	if !strings.HasPrefix(item, "http://") && !strings.HasPrefix(item, "https://") {
		return s.confineLocalPath(item)
	}
	if err := os.MkdirAll(s.cfg.MediaTempDir, 0o755); err != nil {
		return "", done, fmt.Errorf("创建媒体临时目录：%w", err)
	}
	tmp, err := os.CreateTemp(s.cfg.MediaTempDir, "media-*.bin")
	if err != nil {
		return "", done, fmt.Errorf("写临时文件：%w", err)
	}
	done = func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	resp, err := s.fetchURL(ctx, item)
	if err != nil {
		done()
		return "", func() {}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		done()
		return "", func() {}, fmt.Errorf("HTTP %d（图片 CDN 一般需要 Cookie，请检查 imageDownloadCookie 配置）", resp.StatusCode)
	}
	// 多读一个字节用于探测超限，其余按流写入。
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, mediaMaxBytes+1)); err != nil {
		done()
		return "", func() {}, err
	}
	return tmp.Name(), done, nil
}

// fetchURL 逐跳下载：每跳独立校验白名单与解析 IP 并钉住（防 SSRF、DNS 重绑定
// 与重定向绕过），Cookie 只附加给白名单域名。
func (s *Server) fetchURL(ctx context.Context, rawURL string) (*http.Response, error) {
	hop := rawURL
	for n := 0; n <= mediaMaxRedirects; n++ {
		u, err := url.Parse(hop)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, errors.New("图片 URL 无效：仅支持 http/https")
		}
		host := strings.ToLower(u.Hostname())
		if !s.hostAllowed(host) {
			return nil, fmt.Errorf("图片 URL 的域名 %s 不在允许下载的白名单内", host)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("图片 URL 的域名无法解析")
		}
		var pinned net.IP
		for _, a := range ips {
			if !s.cfg.ImageDownloadAllowPrivate && !publicIP(a.IP) {
				return nil, fmt.Errorf("图片 URL 解析到内网地址，已拒绝")
			}
			if pinned == nil {
				pinned = a.IP
			}
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		dialer := &net.Dialer{Timeout: s.cfg.mediaTimeout}
		client := &http.Client{
			Timeout: s.cfg.mediaTimeout,
			Transport: &http.Transport{
				// 钉住已校验的公网 IP；Host 头与 SNI 仍来自 URL 域名。
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(pinned.String(), port))
				},
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, hop, nil)
		if err != nil {
			return nil, err
		}
		if c := strings.TrimSpace(s.cfg.ImageDownloadCookie); c != "" {
			req.Header.Set("Cookie", c)
		}
		req.Header.Set("User-Agent", serverTitle+"/"+serverVersion)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc, err := resp.Location()
			_ = resp.Body.Close()
			if err != nil {
				return nil, errors.New("重定向目标无效")
			}
			hop = loc.String()
			continue
		default:
			return resp, nil
		}
	}
	return nil, errors.New("重定向次数过多")
}

// hostAllowed 判断 host 是否命中下载白名单（后缀匹配，host 已小写化）。
func (s *Server) hostAllowed(host string) bool {
	for _, h := range s.cfg.ImageDownloadHosts {
		if h == "" {
			continue
		}
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// publicIP 判断是否公网地址：拒绝环回、私网、链路本地、组播、未指定、
// CGNAT（100.64/10，含阿里云元数据 100.100.100.200）。
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return !cgnat.Contains(ip)
}

// confineLocalPath 将可选的调用方路径前缀映射到 MediaSourceDir，再校验最终真实路径
// 必须位于该目录内；未配置源目录则拒绝。错误信息统一、不回显路径与 errno。
func (s *Server) confineLocalPath(item string) (string, func(), error) {
	if s.cfg.MediaSourceDir == "" {
		return "", func() {}, errors.New("本地路径不可用：服务未配置媒体源目录，只接受图片 URL")
	}
	root, err := filepath.Abs(s.cfg.MediaSourceDir)
	if err != nil {
		return "", func() {}, errors.New("本地路径不可用")
	}
	cand := item
	// 调用方可能运行在容器内，看到的绝对路径与宿主机不同。仅映射显式配置的
	// 前缀，并保留其下的相对路径；其他绝对路径仍走后续目录收敛并被拒绝。
	if filepath.IsAbs(cand) && s.cfg.MediaSourcePrefix != "" {
		rel, relErr := filepath.Rel(s.cfg.MediaSourcePrefix, filepath.Clean(cand))
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cand = filepath.Join(root, rel)
		}
	}
	// 相对路径按媒体源目录解析（模型常给相对文件名）。
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(root, cand)
	}
	candAbs, err := filepath.Abs(cand)
	if err != nil {
		return "", func() {}, errors.New("本地路径不可用")
	}
	// 根目录与候选路径都消解符号链接后再做包含判断：软链指向目录外必须失败，
	// 不能借软链把目录外内容读进 issue。错误一律回通用文案，不回显路径与 errno。
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", func() {}, errors.New("本地路径不可用")
	}
	real, err := filepath.EvalSymlinks(candAbs)
	if err != nil {
		return "", func() {}, errors.New("本地路径不可用")
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", func() {}, errors.New("本地路径不可用：不在允许的媒体源目录内")
	}
	return real, func() {}, nil
}

// sniffMedia 用魔数嗅探媒体类型，返回 (kind, ext)；kind 取 mediaKindImage / mediaKindVideo。
func sniffMedia(data []byte) (kind, ext string, err error) {
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png":
		return mediaKindImage, "png", nil
	case "image/jpeg":
		return mediaKindImage, "jpg", nil
	case "image/gif":
		return mediaKindImage, "gif", nil
	case "image/webp":
		return mediaKindImage, "webp", nil
	case "video/mp4":
		return mediaKindVideo, "mp4", nil
	case "video/quicktime":
		return mediaKindVideo, "mov", nil
	}
	return "", "", fmt.Errorf("类型不支持（%s）：仅支持 png/jpg/gif/webp 图片与 mp4/mov 视频", ct)
}

// mediaFileName 生成唯一文件名：UTC 时间戳 + 6 字节随机 hex，避免同秒并发冲突。
func mediaFileName(ext string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%x.%s", time.Now().UTC().Format("20060102-150405"), b, ext)
}

// insertMediaSection 把「## 附件」段插到署名行之前（update_issue 整篇替换场景），
// 找不到署名行则追加到文末；署名行前紧邻 --- 分隔线时插到分隔线之前。
func insertMediaSection(body, media string) string {
	idx := strings.LastIndex(body, sigMarker)
	if idx < 0 {
		return body + "\n\n## 附件\n\n" + media + "\n"
	}
	pos := strings.LastIndex(body[:idx], "\n") + 1
	if pos >= 4 && body[pos-4:pos] == "---\n" {
		pos -= 4
	}
	return body[:pos] + "## 附件\n\n" + media + "\n\n" + body[pos:]
}

// mediaReport 输出媒体处理结果播报（create 与 update 共用，避免两处漂移）。
func mediaReport(w *budget, m mediaResult) {
	if m.imageCount+m.videoCount > 0 {
		w.line(fmt.Sprintf("已附带截图 %d 张/视频 %d 个。", m.imageCount, m.videoCount))
	}
	for _, warn := range m.warnings {
		w.line("未能附带：" + warn)
	}
}

// sweepMediaDir 删除媒体临时目录里超过 mediaMaxAge 的孤儿文件，返回清理数量。
func sweepMediaDir(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-mediaMaxAge)
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		// 只清扫本服务创建的临时文件命名空间（os.CreateTemp 的 media-*.bin），
		// 目录里的其他旧文件（用户/运维放的）一概不动。
		if !strings.HasPrefix(e.Name(), "media-") || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			n++
		}
	}
	return n
}

// mediaSweepLoop 启动即清扫一次，之后每小时兜底；随服务生命周期退出。
func (s *Server) mediaSweepLoop(ctx context.Context) {
	sweep := func() {
		if n := sweepMediaDir(s.cfg.MediaTempDir); n > 0 {
			log.Printf("清理媒体孤儿临时文件 %d 个", n)
		}
	}
	sweep()
	t := time.NewTicker(mediaSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// sweepOrphanMedia 清扫服务器持久目录中超过宽限期且无 issue 引用的媒体。
// 判定引用 = 文件名里的随机 hex token 出现在任一 issue 仓的 search 结果。
// 不匹配命名不动、宽限期内不动、引用核验失败不动。
func (s *Server) sweepOrphanMedia(ctx context.Context) {
	if !s.cfg.mediaEnabled() {
		return
	}
	entries, err := os.ReadDir(s.cfg.MediaStoreDir)
	if err != nil {
		log.Printf("媒体孤儿清扫跳过（读取目录失败）：%v", err)
		return
	}
	repos := s.issueRepos(false)
	if len(repos) == 0 {
		return
	}
	cutoff := time.Now().Add(-mediaOrphanGrace)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		name := entry.Name()
		m := reMediaName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		ts, err := time.ParseInLocation("20060102-150405", m[1], time.UTC)
		if err != nil || ts.After(cutoff) {
			continue
		}
		ref, err := s.mediaReferenced(ctx, repos, m[2])
		if err != nil {
			log.Printf("媒体孤儿清扫：%s 引用核验失败（跳过）：%v", name, err)
			continue
		}
		if ref {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.MediaStoreDir, name)); err != nil {
			log.Printf("媒体孤儿清扫：删除 %s 失败：%v", name, err)
			continue
		}
		log.Printf("清理孤儿媒体 %s（无人引用且超过 %d 天）", name, int(mediaOrphanGrace.Hours()/24))
	}
}

// mediaReferenced 核验 hex token 是否被任何 issue 引用。有全局令牌时先做全局
// 检索（覆盖未配置仓库里的引用），再逐仓核对；任一必需查询失败都返回错误。
func (s *Server) mediaReferenced(ctx context.Context, repos []*Repo, hexToken string) (bool, error) {
	if s.cfg.GitHubToken != "" {
		hit, err := s.gh.MediaReferencedGlobal(ctx, s.cfg.GitHubToken, hexToken)
		if err != nil {
			return false, err
		}
		if hit {
			return true, nil
		}
	}
	for _, r := range repos {
		hit, err := s.gh.MediaReferenced(ctx, r.Slug, r.GHToken, hexToken)
		if err != nil {
			return false, err
		}
		if hit {
			return true, nil
		}
	}
	return false, nil
}

// mediaStoreSweepLoop 启动即清扫一次，之后每天兜底；随服务生命周期退出。
func (s *Server) mediaStoreSweepLoop(ctx context.Context) {
	s.sweepOrphanMedia(ctx)
	t := time.NewTicker(mediaStoreSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOrphanMedia(ctx)
		}
	}
}
