// media.go：issue 媒体接收、临时 staging 与 GitHub 原生附件渲染。
//
// create_issue / update_issue 的 images 参数（本地路径或 URL 列表）由服务端
// 下载或受限读取 → 类型/大小校验 → 流式上传到 GitHub user-attachments。
// repoMcp 不永久托管媒体，也不删除 AstrBot 拥有的源文件。
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
//   - 上传额度：每仓每小时 mediaUploadLimit 项（默认 20），独立于 issue 创建限额。
//
// 失败语义：媒体是正文的辅助信息，单个或全部附件失败都不阻断 issue 创建/更新；
// 成功项保留，失败项通过工具返回明确报告。
//
// 临时文件：URL 下载走 <MediaTempDir> 流式落盘，上传成功/失败都立即删除；
// 进程崩溃遗留的下载文件由 mediaSweepLoop 按 mediaMaxAge 兜底清扫。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	mediaKindImage = "image"
	mediaKindVideo = "video"
)

type attachmentUploader interface {
	Upload(context.Context, string, int64, attachmentInput) (uploadedAttachment, error)
}

// mediaResult 是单次媒体处理的产物。单项失败只增加 failed/warnings，
// 已通过校验并上传成功的原生附件仍按输入顺序渲染进正文。
type mediaResult struct {
	md         string
	requested  int
	imageCount int
	videoCount int
	failed     int
	warnings   []string
}

// processMedia 校验 images 列表并上传到 GitHub 原生 user-attachments。
// 单项或全部失败都不阻断 issue 创建/更新。
func (s *Server) processMedia(ctx context.Context, repo *Repo, list []string) mediaResult {
	res := mediaResult{requested: len(list)}
	if len(list) == 0 {
		return res
	}
	if len(list) > mediaMaxCount {
		dropped := len(list) - mediaMaxCount
		res.failed += dropped
		res.warnings = append(res.warnings,
			fmt.Sprintf("附件超过 %d 个，后 %d 个未处理", mediaMaxCount, dropped))
		list = list[:mediaMaxCount]
	}
	if s.attachmentUploader == nil {
		res.failed += len(list)
		res.warnings = append(res.warnings, "GitHub 原生附件凭据未配置或未认证")
		return res
	}
	if repo == nil || repo.Slug == "" || s.gh == nil {
		res.failed += len(list)
		res.warnings = append(res.warnings, "目标仓库不可用于 GitHub 原生附件上传")
		return res
	}
	repoID, err := s.gh.RepoID(ctx, repo.GHToken, repo.Slug)
	if err != nil {
		res.failed += len(list)
		res.warnings = append(res.warnings, fmt.Sprintf("无法读取目标仓库信息：%v", err))
		return res
	}

	var b strings.Builder
	for i, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 为空", i+1))
			continue
		}
		path, done, err := s.stageMedia(ctx, item)
		if err != nil {
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 下载失败：%v", i+1, err))
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			done()
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		kind, ext, err := sniffMedia(head[:n])
		if err != nil {
			_ = f.Close()
			done()
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d %v", i+1, err))
			continue
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			done()
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		if st.Size() > mediaMaxBytes {
			_ = f.Close()
			done()
			res.failed++
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
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
			continue
		}
		width, height := 0, 0
		if kind == mediaKindImage {
			if imageCfg, _, decodeErr := image.DecodeConfig(f); decodeErr == nil {
				width, height = imageCfg.Width, imageCfg.Height
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				_ = f.Close()
				done()
				res.failed++
				res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 读取失败（文件不可用）", i+1))
				continue
			}
		}
		if s.mediaLimiter != nil {
			if ok, wait := s.mediaLimiter.take(repo.Name); !ok {
				_ = f.Close()
				done()
				res.failed += len(list) - i
				res.warnings = append(res.warnings, fmt.Sprintf(
					"媒体上传达到每小时上限（%d 项），约 %d 分钟后可再试",
					s.cfg.mediaUploadLimit, int(wait.Minutes())+1))
				break
			}
		}
		uploaded, err := s.attachmentUploader.Upload(ctx, repo.Slug, repoID, attachmentInput{
			Name:        mediaFileName(ext),
			ContentType: mediaContentType(ext),
			Size:        st.Size(),
			Reader:      f,
		})
		_ = f.Close()
		done()
		if err != nil {
			if isAttachmentAuthenticationError(err) {
				s.attachmentStatus.markUnauthenticated()
			}
			res.failed++
			res.warnings = append(res.warnings, fmt.Sprintf("附件 %d 上传失败：%v", i+1, err))
			continue
		}
		if kind == mediaKindVideo {
			b.WriteString(uploaded.URL)
			b.WriteByte('\n')
			res.videoCount++
			continue
		}
		if width > 0 && height > 0 {
			fmt.Fprintf(&b, `<img width="%d" height="%d" alt="Image" src="%s" />`+"\n",
				width, height, html.EscapeString(uploaded.URL))
		} else {
			fmt.Fprintf(&b, `<img alt="Image" src="%s" />`+"\n", html.EscapeString(uploaded.URL))
		}
		res.imageCount++
	}
	res.md = strings.TrimSpace(b.String())
	return res
}

func mediaContentType(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "jpg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	default:
		return "application/octet-stream"
	}
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
	if m.requested == 0 {
		return
	}
	success := m.imageCount + m.videoCount
	w.line(fmt.Sprintf("请求附件 %d 个：成功 %d 个，失败 %d 个。", m.requested, success, m.failed))
	for _, warn := range m.warnings {
		w.line("附件缺失：" + warn)
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
