package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxAttachmentSessionBytes = 4096
const maxGitHubHTMLBytes = 1 << 20

type attachmentStatus struct {
	Configured    bool   `json:"configured"`
	Authenticated bool   `json:"authenticated"`
	Account       string `json:"account,omitempty"`
}

type attachmentStatusCache struct {
	mu     sync.RWMutex
	status attachmentStatus
}

func newAttachmentStatusCache(initial attachmentStatus) *attachmentStatusCache {
	return &attachmentStatusCache{status: initial}
}

func (c *attachmentStatusCache) load() attachmentStatus {
	if c == nil {
		return attachmentStatus{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *attachmentStatusCache) markUnauthenticated() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.status.Authenticated = false
	c.mu.Unlock()
}

func isAttachmentAuthenticationError(err error) bool {
	return errors.Is(err, errAttachmentSessionExpired) ||
		errors.Is(err, errAttachmentAccountMismatch) ||
		errors.Is(err, errAttachmentAccountUnknown)
}

type nativeAttachmentUploader struct {
	githubBase                *url.URL
	githubClient              *http.Client
	uploadClient              *http.Client
	validateUploadDestination func(*url.URL) error
	session                   string
	expectedAccount           string
}

var reGitHubUserLogin = regexp.MustCompile(`<meta\s+name=["']user-login["']\s+content=["']([^"']+)["'][^>]*>`)

var errAttachmentSessionExpired = errors.New("GitHub 原生附件 Session 已失效")
var errAttachmentAccountMismatch = errors.New("GitHub 原生附件账号不符")
var errAttachmentAccountUnknown = errors.New("无法确认 GitHub 原生附件账号")

func loadAttachmentSession(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("读取 GitHub 附件 Session 文件 %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("GitHub 附件 Session 文件 %s 必须是普通文件且不能是软链", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("GitHub 附件 Session 文件 %s 权限必须为 0600", path)
	}
	if info.Size() <= 0 || info.Size() > maxAttachmentSessionBytes {
		return "", fmt.Errorf("GitHub 附件 Session 文件 %s 大小非法", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 GitHub 附件 Session 文件 %s: %w", path, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxAttachmentSessionBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取 GitHub 附件 Session 文件 %s: %w", path, err)
	}
	if len(raw) > maxAttachmentSessionBytes {
		return "", fmt.Errorf("GitHub 附件 Session 文件 %s 过大", path)
	}
	session := strings.TrimSpace(string(raw))
	if session == "" || strings.ContainsAny(session, "\r\n\x00") {
		return "", fmt.Errorf("GitHub 附件 Session 文件 %s 内容非法", path)
	}
	return session, nil
}

func newNativeAttachmentUploader(cfg *Config) (*nativeAttachmentUploader, attachmentStatus, error) {
	status := attachmentStatus{}
	if cfg == nil || cfg.GitHubAttachmentSessionFile == "" || cfg.GitHubAttachmentAccount == "" {
		return nil, status, nil
	}
	status.Configured = true
	status.Account = cfg.GitHubAttachmentAccount
	session, err := loadAttachmentSession(cfg.GitHubAttachmentSessionFile)
	if err != nil {
		return nil, status, err
	}
	base, err := url.Parse("https://github.com")
	if err != nil {
		return nil, status, err
	}
	githubTimeout := cfg.ghTimeout
	if githubTimeout <= 0 {
		githubTimeout = 20 * time.Second
	}
	uploadTimeout := cfg.mediaTimeout
	if uploadTimeout <= 0 {
		uploadTimeout = 60 * time.Second
	}
	uploader, err := buildNativeAttachmentUploader(
		session,
		cfg.GitHubAttachmentAccount,
		base,
		&http.Client{Timeout: githubTimeout},
		&http.Client{Timeout: uploadTimeout},
	)
	if err != nil {
		return nil, status, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubTimeout)
	defer cancel()
	account, err := uploader.checkIdentity(ctx)
	if err != nil {
		return nil, status, fmt.Errorf("验证 GitHub 原生附件账号: %w", err)
	}
	status.Authenticated = true
	status.Account = account
	return uploader, status, nil
}

func buildNativeAttachmentUploader(session, expectedAccount string, githubBase *url.URL, githubClient, uploadClient *http.Client) (*nativeAttachmentUploader, error) {
	session = strings.TrimSpace(session)
	expectedAccount = strings.TrimSpace(expectedAccount)
	if session == "" || expectedAccount == "" {
		return nil, errors.New("GitHub 原生附件 Session 与账号不能为空")
	}
	if githubBase == nil || githubBase.Scheme != "https" || githubBase.Host == "" || githubBase.User != nil {
		return nil, errors.New("GitHub 原生附件网页地址必须是无凭据的 HTTPS URL")
	}
	if githubClient == nil {
		githubClient = &http.Client{}
	}
	githubClone := *githubClient
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("创建 GitHub Cookie Jar: %w", err)
	}
	githubClone.Jar = jar
	githubClone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if uploadClient == nil {
		uploadClient = &http.Client{}
	}
	uploadClone := *uploadClient
	uploadClone.Jar = nil
	uploadClone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	uploader := &nativeAttachmentUploader{
		githubBase:                githubBase,
		githubClient:              &githubClone,
		uploadClient:              &uploadClone,
		validateUploadDestination: validateGitHubUploadDestination,
		session:                   session,
		expectedAccount:           expectedAccount,
	}
	uploader.githubClient.Jar.SetCookies(githubBase, []*http.Cookie{
		{Name: "user_session", Value: session, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
		{Name: "__Host-user_session_same_site", Value: session, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
	})
	return uploader, nil
}

func (u *nativeAttachmentUploader) checkIdentity(ctx context.Context) (string, error) {
	if u == nil || u.githubBase == nil || u.githubClient == nil {
		return "", errors.New("GitHub 原生附件上传器未初始化")
	}
	endpoint := u.githubBase.ResolveReference(&url.URL{Path: "/settings/profile"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("创建 GitHub 身份检查请求: %w", err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "repoMcp/0.1.0")
	resp, err := u.githubClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 GitHub 身份页: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther {
		return "", errAttachmentSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub 身份检查返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubHTMLBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取 GitHub 身份页: %w", err)
	}
	if len(body) > maxGitHubHTMLBytes {
		return "", errors.New("GitHub 身份页超过响应上限")
	}
	match := reGitHubUserLogin.FindSubmatch(body)
	if len(match) != 2 {
		return "", errAttachmentAccountUnknown
	}
	account := html.UnescapeString(string(match[1]))
	if account != u.expectedAccount {
		return "", fmt.Errorf("%w：期望 %s，实际 %s", errAttachmentAccountMismatch, u.expectedAccount, account)
	}
	return account, nil
}

func (u *nativeAttachmentUploader) classifyAuthenticationFailure(ctx context.Context, fallback error) error {
	_, identityErr := u.checkIdentity(ctx)
	if isAttachmentAuthenticationError(identityErr) {
		return identityErr
	}
	return fallback
}

type attachmentInput struct {
	Name        string
	ContentType string
	Size        int64
	Reader      io.Reader
}

type uploadedAttachment struct {
	Name string
	URL  string
}

type uploadPolicyResponse struct {
	UploadURL string `json:"upload_url"`
	Asset     struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
		Href        string `json:"href"`
	} `json:"asset"`
	Form                         map[string]string `json:"form"`
	AssetUploadURL               string            `json:"asset_upload_url"`
	AssetUploadAuthenticityToken string            `json:"asset_upload_authenticity_token"`
}

type multipartField struct {
	Name  string
	Value string
}

var reRepositoryUploadToken = regexp.MustCompile(`"uploadToken":"([^"]+)"`)
var reNativeAttachmentPath = regexp.MustCompile(`^/user-attachments/assets/[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var reGitHubS3Host = regexp.MustCompile(`^github-production-[a-z0-9-]+\.s3(?:\.[a-z0-9-]+)?\.amazonaws\.com$`)

func (u *nativeAttachmentUploader) Upload(ctx context.Context, repoSlug string, repoID int64, in attachmentInput) (uploadedAttachment, error) {
	if u == nil || u.githubClient == nil || u.uploadClient == nil {
		return uploadedAttachment{}, errors.New("GitHub 原生附件上传器未初始化")
	}
	if !reRepoSlug.MatchString(repoSlug) || repoID <= 0 {
		return uploadedAttachment{}, errors.New("GitHub 原生附件目标仓库无效")
	}
	if in.Reader == nil || in.Size <= 0 || in.Size > mediaMaxBytes ||
		in.Name == "" || strings.ContainsAny(in.Name, "/\\\r\n\x00") ||
		(!strings.HasPrefix(in.ContentType, "image/") && !strings.HasPrefix(in.ContentType, "video/")) {
		return uploadedAttachment{}, errors.New("GitHub 原生附件输入无效")
	}
	uploadToken, err := u.repositoryUploadToken(ctx, repoSlug)
	if err != nil {
		return uploadedAttachment{}, err
	}
	policy, err := u.requestUploadPolicy(ctx, repoSlug, repoID, in, uploadToken)
	if err != nil {
		return uploadedAttachment{}, err
	}
	finalizeURL, err := u.resolveFinalizeURL(policy.AssetUploadURL, policy.Asset.ID)
	if err != nil {
		return uploadedAttachment{}, err
	}
	if err := u.uploadToObjectStore(ctx, policy, in); err != nil {
		return uploadedAttachment{}, err
	}
	return u.finalizeAttachment(ctx, repoSlug, finalizeURL, policy.AssetUploadAuthenticityToken)
}

func (u *nativeAttachmentUploader) repositoryUploadToken(ctx context.Context, repoSlug string) (string, error) {
	endpoint := u.githubBase.ResolveReference(&url.URL{Path: "/" + repoSlug})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("创建 GitHub 仓库页请求: %w", err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", serverTitle+"/"+serverVersion)
	resp, err := u.githubClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 GitHub 仓库页: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther {
		return "", errAttachmentSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub 仓库页返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubHTMLBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取 GitHub 仓库页: %w", err)
	}
	if len(body) > maxGitHubHTMLBytes {
		return "", errors.New("GitHub 仓库页超过响应上限")
	}
	match := reRepositoryUploadToken.FindSubmatch(body)
	if len(match) != 2 {
		return "", u.classifyAuthenticationFailure(
			ctx,
			errors.New("GitHub 原生附件接口不兼容：仓库页缺少 uploadToken"),
		)
	}
	return string(match[1]), nil
}

func (u *nativeAttachmentUploader) requestUploadPolicy(ctx context.Context, repoSlug string, repoID int64, in attachmentInput, uploadToken string) (uploadPolicyResponse, error) {
	fields := []multipartField{
		{Name: "name", Value: in.Name},
		{Name: "size", Value: strconv.FormatInt(in.Size, 10)},
		{Name: "content_type", Value: in.ContentType},
		{Name: "authenticity_token", Value: uploadToken},
		{Name: "repository_id", Value: strconv.FormatInt(repoID, 10)},
	}
	body, contentType, err := bufferedMultipart(fields)
	if err != nil {
		return uploadPolicyResponse{}, fmt.Errorf("构造 GitHub 附件 policy 请求: %w", err)
	}
	endpoint := u.githubBase.ResolveReference(&url.URL{Path: "/upload/policies/assets"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return uploadPolicyResponse{}, fmt.Errorf("创建 GitHub 附件 policy 请求: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	u.setBrowserHeaders(req, repoSlug)
	resp, err := u.githubClient.Do(req)
	if err != nil {
		return uploadPolicyResponse{}, fmt.Errorf("请求 GitHub 附件 policy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return uploadPolicyResponse{}, u.classifyAuthenticationFailure(
			ctx,
			fmt.Errorf("GitHub 附件 policy 返回 HTTP %d", resp.StatusCode),
		)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxRespBytes+1))
	if err != nil {
		return uploadPolicyResponse{}, fmt.Errorf("读取 GitHub 附件 policy: %w", err)
	}
	if len(raw) > ghMaxRespBytes {
		return uploadPolicyResponse{}, errors.New("GitHub 附件 policy 响应超过上限")
	}
	var policy uploadPolicyResponse
	if err := json.Unmarshal(raw, &policy); err != nil {
		return uploadPolicyResponse{}, errors.New("GitHub 原生附件接口不兼容：policy 响应不是有效 JSON")
	}
	if policy.UploadURL == "" || len(policy.Form) == 0 || policy.Asset.ID <= 0 ||
		policy.AssetUploadURL == "" || policy.AssetUploadAuthenticityToken == "" {
		return uploadPolicyResponse{}, errors.New("GitHub 原生附件接口不兼容：policy 响应字段不完整")
	}
	return policy, nil
}

func bufferedMultipart(fields []multipartField) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}

func streamingMultipart(fields []multipartField, fileField, fileName string, file io.Reader, fileSize int64) (io.Reader, string, int64, error) {
	if file == nil || fileSize < 0 || fileField == "" || fileName == "" {
		return nil, "", 0, errors.New("multipart 文件参数无效")
	}
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	for _, field := range fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			return nil, "", 0, err
		}
	}
	if _, err := writer.CreateFormFile(fileField, fileName); err != nil {
		return nil, "", 0, err
	}
	suffix := []byte("\r\n--" + writer.Boundary() + "--\r\n")
	contentLength := int64(prefix.Len()) + fileSize + int64(len(suffix))
	if contentLength < fileSize {
		return nil, "", 0, errors.New("multipart Content-Length 溢出")
	}
	body := io.MultiReader(bytes.NewReader(prefix.Bytes()), file, bytes.NewReader(suffix))
	return body, writer.FormDataContentType(), contentLength, nil
}

func (u *nativeAttachmentUploader) uploadToObjectStore(ctx context.Context, policy uploadPolicyResponse, in attachmentInput) error {
	uploadURL, err := url.Parse(policy.UploadURL)
	if err != nil {
		return errors.New("GitHub 原生附件接口不兼容：upload_url 无效")
	}
	validator := u.validateUploadDestination
	if validator == nil {
		validator = validateGitHubUploadDestination
	}
	if err := validator(uploadURL); err != nil {
		return fmt.Errorf("GitHub 原生附件上传目标被拒绝: %w", err)
	}
	fields, err := orderedObjectStoreFields(policy.Form)
	if err != nil {
		return err
	}
	body, contentType, contentLength, err := streamingMultipart(fields, "file", in.Name, in.Reader, in.Size)
	if err != nil {
		return fmt.Errorf("构造 GitHub 对象存储请求: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), body)
	if err != nil {
		return fmt.Errorf("创建 GitHub 对象存储请求: %w", err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", u.origin())
	resp, err := u.uploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("上传 GitHub 附件对象: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("GitHub 附件对象存储返回 HTTP %d", resp.StatusCode)
	}
}

func orderedObjectStoreFields(form map[string]string) ([]multipartField, error) {
	values := make(map[string]string, len(form))
	for key, value := range form {
		values[key] = value
	}
	order := []string{
		"key", "acl", "policy", "X-Amz-Algorithm", "X-Amz-Credential",
		"X-Amz-Date", "X-Amz-Signature", "Content-Type", "Cache-Control",
		"x-amz-meta-Surrogate-Control",
	}
	required := map[string]bool{
		"key": true, "policy": true, "X-Amz-Algorithm": true,
		"X-Amz-Credential": true, "X-Amz-Date": true, "X-Amz-Signature": true,
	}
	fields := make([]multipartField, 0, len(form))
	for _, key := range order {
		value, ok := values[key]
		if !ok {
			if required[key] {
				return nil, errors.New("GitHub 原生附件接口不兼容：对象存储表单字段不完整")
			}
			continue
		}
		fields = append(fields, multipartField{Name: key, Value: value})
		delete(values, key)
	}
	extra := make([]string, 0, len(values))
	for key := range values {
		extra = append(extra, key)
	}
	sort.Strings(extra)
	for _, key := range extra {
		fields = append(fields, multipartField{Name: key, Value: values[key]})
	}
	return fields, nil
}

func (u *nativeAttachmentUploader) resolveFinalizeURL(raw string, assetID int64) (*url.URL, error) {
	relative, err := url.Parse(raw)
	expectedPath := "/upload/assets/" + strconv.FormatInt(assetID, 10)
	if err != nil || assetID <= 0 || raw != expectedPath || relative.IsAbs() || relative.Host != "" ||
		relative.User != nil || relative.RawQuery != "" || relative.Fragment != "" ||
		relative.Path != expectedPath {
		return nil, errors.New("GitHub 原生附件接口不兼容：finalize URL 无效")
	}
	return u.githubBase.ResolveReference(relative), nil
}

func (u *nativeAttachmentUploader) finalizeAttachment(ctx context.Context, repoSlug string, endpoint *url.URL, token string) (uploadedAttachment, error) {
	body, contentType, err := bufferedMultipart([]multipartField{{Name: "authenticity_token", Value: token}})
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("构造 GitHub 附件 finalize 请求: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), body)
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("创建 GitHub 附件 finalize 请求: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	u.setBrowserHeaders(req, repoSlug)
	resp, err := u.githubClient.Do(req)
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("完成 GitHub 原生附件上传: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return uploadedAttachment{}, u.classifyAuthenticationFailure(
			ctx,
			fmt.Errorf("GitHub 附件 finalize 返回 HTTP %d", resp.StatusCode),
		)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxRespBytes+1))
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("读取 GitHub 附件 finalize 响应: %w", err)
	}
	if len(raw) > ghMaxRespBytes {
		return uploadedAttachment{}, errors.New("GitHub 附件 finalize 响应超过上限")
	}
	var result struct {
		Href string `json:"href"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return uploadedAttachment{}, errors.New("GitHub 原生附件接口不兼容：finalize 响应不是有效 JSON")
	}
	validated, err := validateNativeAttachmentURL(result.Href)
	if err != nil {
		return uploadedAttachment{}, err
	}
	if strings.TrimSpace(result.Name) == "" {
		return uploadedAttachment{}, errors.New("GitHub 原生附件接口不兼容：finalize 响应缺少文件名")
	}
	return uploadedAttachment{Name: result.Name, URL: validated.String()}, nil
}

func validateNativeAttachmentURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !reNativeAttachmentPath.MatchString(parsed.Path) ||
		parsed.EscapedPath() != parsed.Path {
		return nil, errors.New("GitHub 原生附件接口返回了非法附件 URL")
	}
	return parsed, nil
}

func validateGitHubUploadDestination(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.User != nil || target.Port() != "" ||
		target.RawQuery != "" || target.Fragment != "" || !reGitHubS3Host.MatchString(strings.ToLower(target.Hostname())) {
		return errors.New("对象存储地址不在 GitHub S3 白名单")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, target.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("GitHub 对象存储域名无法解析")
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			return errors.New("GitHub 对象存储域名解析到非公网地址")
		}
	}
	return nil
}

func (u *nativeAttachmentUploader) setBrowserHeaders(req *http.Request, repoSlug string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", u.origin())
	req.Header.Set("Referer", u.githubBase.ResolveReference(&url.URL{Path: "/" + repoSlug}).String())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", serverTitle+"/"+serverVersion)
}

func (u *nativeAttachmentUploader) origin() string {
	return u.githubBase.Scheme + "://" + u.githubBase.Host
}
