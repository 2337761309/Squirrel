package checker

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"subdomain-checker/config"
	"subdomain-checker/screenshot"

	"github.com/chromedp/chromedp"
	"github.com/fogleman/gg"
)

// 子域名检测结果
type Result struct {
	Domain       string
	Status       int
	Alive        bool
	StatusText   string // 状态文本，如"存活"、"404"、"403"等
	Message      string
	ResponseTime time.Duration
	PageInfo     *PageType // 页面信息
	Title        string    // 页面标题
	Screenshot   string    // 保存的截图文件名
}

// 配置项
type Config struct {
	Timeout          int
	Concurrency      int
	Verbose          bool
	FollowRedirects  bool
	ShowResponseTime bool
	OutputFile       string
	ExcelFile        string // 添加 Excel 输出文件配置
	ExtractInfo      bool   // 是否提取页面重要信息
	OnlyAlive        bool   // 是否只导出存活的域名
	Screenshot       bool   // 是否对所有网页进行截图
	ScreenshotAlive  bool   // 是否只截图存活的网页
	ScreenshotDir    string // 截图保存目录
}

// 页面类型
type PageType struct {
	Type        string // 页面类型：登录页面、后台页面等
	Description string // 更详细的描述
}

// 截图任务
type ScreenshotTask struct {
	URL      string
	Filename string
	Dir      string
	Result   chan<- string // 返回截图路径或空字符串（失败时）
}

// 截图工作池
type ScreenshotPool struct {
	tasks   chan ScreenshotTask
	workers int
	wg      sync.WaitGroup
}

// 创建新的截图工作池
func NewScreenshotPool(workers int) *ScreenshotPool {
	return &ScreenshotPool{
		tasks:   make(chan ScreenshotTask, workers*2), // 缓冲大小为工作者数量的2倍
		workers: workers,
	}
}

// 启动截图工作池
func (p *ScreenshotPool) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(workerId int) {
			defer p.wg.Done()

			// 为每个工作者创建一个chromedp实例
			opts := append(chromedp.DefaultExecAllocatorOptions[:],
				chromedp.Flag("headless", true),
				chromedp.Flag("disable-gpu", true),
				chromedp.Flag("no-sandbox", true),
				chromedp.Flag("disable-dev-shm-usage", true),
				chromedp.WindowSize(1920, 1080),
			)
			allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
			defer cancel()

			ctx, cancel := chromedp.NewContext(allocCtx)
			defer cancel()

			for task := range p.tasks {
				screenshotPath := filepath.Join(task.Dir, task.Filename)

				// 执行截图
				if err := takeScreenshotWithContext(ctx, task.URL, screenshotPath); err == nil {
					task.Result <- screenshotPath
				} else {
					// 截图失败，生成错误图片
					errorFilename := "error_" + task.Filename
					errorPath := filepath.Join(task.Dir, errorFilename)
					generateErrorImage(errorFilename, task.Dir)
					task.Result <- errorPath
				}
			}
		}(i)
	}
}

// 提交截图任务
func (p *ScreenshotPool) Submit(url, filename, dir string) <-chan string {
	result := make(chan string, 1)
	p.tasks <- ScreenshotTask{
		URL:      url,
		Filename: filename,
		Dir:      dir,
		Result:   result,
	}
	return result
}

// 关闭截图工作池
func (p *ScreenshotPool) Stop() {
	close(p.tasks)
	p.wg.Wait()
}

// 使用已有的context进行截图
func takeScreenshotWithContext(ctx context.Context, url string, screenshotPath string) error {
	// 准备截图缓冲区
	var buf []byte

	// 检查URL是否包含协议前缀
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}

	// 设置超时
	taskCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// 执行截图
	tasks := chromedp.Tasks{
		chromedp.Navigate(url),
		// 等待页面加载
		chromedp.Sleep(2 * time.Second),

		// 获取页面滚动高度
		chromedp.Evaluate(`Math.max(document.body.scrollHeight, document.documentElement.scrollHeight)`, nil),

		// 使用FullScreenshot
		chromedp.FullScreenshot(&buf, 90), // 稍微降低质量以提高性能
	}

	if err := chromedp.Run(taskCtx, tasks); err != nil {
		return err
	}

	// 保存截图到文件
	return os.WriteFile(screenshotPath, buf, 0644)
}

// 检查域名是否存活
func CheckDomain(domain string, cfg config.Config, resultChan chan<- Result, screenshotPool *screenshot.ScreenshotPool) {
	// 如果已经指定了协议，直接使用
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		checkSingleDomain(domain, cfg, resultChan, screenshotPool)
		return
	}

	// 未指定协议，先尝试HTTPS
	httpsDomain := "https://" + domain
	httpsResult := Result{
		Domain: httpsDomain,
		Alive:  false,
	}

	// 创建一个带有连接池的客户端
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false, // 启用keep-alive
	}

	client := &http.Client{
		Timeout:   time.Duration(cfg.Timeout) * time.Second,
		Transport: transport,
	}

	// 处理重定向
	if !cfg.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	startTime := time.Now()
	resp, err := client.Get(httpsDomain)
	responseTime := time.Since(startTime)
	httpsResult.ResponseTime = responseTime

	if err == nil {
		defer resp.Body.Close()
		httpsResult.Status = resp.StatusCode

		// 根据状态码设置状态文本和存活标志
		httpsResult.StatusText, httpsResult.Alive = getStatusTextAndAlive(resp.StatusCode)
		httpsResult.Message = http.StatusText(resp.StatusCode)

		// 提取页面信息
		if resp.StatusCode < 400 {
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				pageContent := string(body)
				if cfg.ExtractInfo {
					httpsResult.PageInfo = detectPageType(pageContent)
				}
				httpsResult.Title = extractTitle(pageContent)
			}
		}

		// 如果需要截图，使用截图工作池
		if screenshotPool != nil && (cfg.Screenshot || cfg.ScreenshotAlive) {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(httpsDomain)

			// 确保截图目录存在
			if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(httpsDomain, screenFilename, cfg.ScreenshotDir)

				// 等待截图结果
				if screenshotPath := <-resultCh; screenshotPath != "" {
					// 将完整路径转换为相对路径
					relPath := filepath.Join("screenshots", filepath.Base(screenshotPath))
					// 确保使用正斜杠
					relPath = strings.ReplaceAll(relPath, "\\", "/")
					httpsResult.Screenshot = relPath
				}
			}
		}

		resultChan <- httpsResult
		return
	}

	// HTTPS请求失败，尝试HTTP
	httpDomain := "http://" + domain
	checkSingleDomain(httpDomain, cfg, resultChan, screenshotPool)
}

// 使用指定协议检查单个域名
func checkSingleDomain(domain string, cfg config.Config, resultChan chan<- Result, screenshotPool *screenshot.ScreenshotPool) {
	result := Result{
		Domain: domain,
		Alive:  false,
	}

	// 创建一个带有连接池的客户端
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false, // 启用keep-alive
	}

	client := &http.Client{
		Timeout:   time.Duration(cfg.Timeout) * time.Second,
		Transport: transport,
	}

	// 处理重定向
	if !cfg.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	startTime := time.Now()
	resp, err := client.Get(domain)
	responseTime := time.Since(startTime)
	result.ResponseTime = responseTime

	if err != nil {
		result.Message = err.Error()
		result.StatusText = "无法访问"
		resultChan <- result
		return
	}
	defer resp.Body.Close()

	result.Status = resp.StatusCode

	// 根据状态码设置状态文本和存活标志
	result.StatusText, result.Alive = getStatusTextAndAlive(resp.StatusCode)
	result.Message = http.StatusText(resp.StatusCode)

	// 提取页面信息
	if resp.StatusCode < 400 {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			pageContent := string(body)
			if cfg.ExtractInfo {
				result.PageInfo = detectPageType(pageContent)
			}
			result.Title = extractTitle(pageContent)
		}
	}

	// 如果需要截图，使用截图工作池
	if screenshotPool != nil && (cfg.Screenshot || cfg.ScreenshotAlive) {
		// 为网站生成唯一的截图文件名
		screenFilename := generateScreenshotFilename(domain)

		// 确保截图目录存在
		if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
			// 提交截图任务到工作池
			resultCh := screenshotPool.Submit(domain, screenFilename, cfg.ScreenshotDir)

			// 等待截图结果
			if screenshotPath := <-resultCh; screenshotPath != "" {
				// 将完整路径转换为相对路径
				relPath := filepath.Join("screenshots", filepath.Base(screenshotPath))
				// 确保使用正斜杠
				relPath = strings.ReplaceAll(relPath, "\\", "/")
				result.Screenshot = relPath
			}
		}
	}

	resultChan <- result
}

// 根据状态码返回对应的状态文本和是否存活
func getStatusTextAndAlive(statusCode int) (string, bool) {
	switch {
	case statusCode == 200:
		return "存活", true
	case statusCode == 301 || statusCode == 302:
		return "重定向", true
	case statusCode == 403:
		return "禁止访问", false
	case statusCode == 404:
		return "未找到", false
	case statusCode == 500:
		return "服务器错误", false
	case statusCode == 502:
		return "网关错误", false
	case statusCode == 503:
		return "服务不可用", false
	case statusCode < 400:
		return "存活", true
	default:
		return "无法访问", false
	}
}

// 检测页面类型
func detectPageType(content string) *PageType {
	lowerContent := strings.ToLower(content)

	// 检测登录页面
	if containsAny(lowerContent, []string{
		"<form.*login", "login.*<form", "sign in", "signin",
		"username.*password", "userid.*password", "用户名.*密码",
		"登录", "登陆", "login_form", "input.*password",
	}) {
		return &PageType{
			Type:        "登录页面",
			Description: "可能含有用户名和密码输入框",
		}
	}

	// 检测管理后台
	if containsAny(lowerContent, []string{
		"admin", "manage", "dashboard", "console",
		"control panel", "cpanel", "后台管理", "管理系统", "系统管理",
	}) {
		return &PageType{
			Type:        "管理后台",
			Description: "可能是系统管理界面",
		}
	}

	// 检测API接口
	if containsAny(lowerContent, []string{
		"api", "swagger", "graphql", "endpoint", "json",
	}) || strings.Contains(content, "{\"") || strings.Contains(content, "[{\"") {
		return &PageType{
			Type:        "API接口",
			Description: "可能是API接口或文档",
		}
	}

	// 检测上传功能
	if containsAny(lowerContent, []string{
		"upload", "file", "browse", "上传", "文件",
		"<input.*type=\"file\"", "multipart/form-data",
	}) {
		return &PageType{
			Type:        "上传页面",
			Description: "含有文件上传功能",
		}
	}

	return nil
}

// 提取页面标题
func extractTitle(content string) string {
	titleRegex := regexp.MustCompile(`<title[^>]*>(.*?)</title>`)
	matches := titleRegex.FindStringSubmatch(content)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

// 检查内容是否包含任何指定的字符串
func containsAny(content string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}
	return false
}

// 生成截图文件名
func generateScreenshotFilename(domain string) string {
	// 将域名中的特殊字符替换为下划线
	filename := strings.ReplaceAll(domain, "://", "_")
	filename = strings.ReplaceAll(filename, ".", "_")
	filename = strings.ReplaceAll(filename, ":", "_")
	filename = strings.ReplaceAll(filename, "/", "_")
	return filename + ".png"
}

// 生成错误图片（当无法截图时）
func generateErrorImage(filename string, screenshotDir string) error {
	// 创建截图目录（如果不存在）
	if err := os.MkdirAll(screenshotDir, 0755); err != nil {
		return fmt.Errorf("创建截图目录失败: %v", err)
	}

	// 生成一个简单的错误图片
	width, height := 800, 600
	upLeft := image.Point{0, 0}
	lowRight := image.Point{width, height}

	img := image.NewRGBA(image.Rectangle{upLeft, lowRight})

	// 填充白色背景
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.White)
		}
	}

	// 添加红色错误文本
	fontColor := color.RGBA{255, 0, 0, 255} // 红色
	errorPath := filepath.Join(screenshotDir, filename)

	// 创建图片对象
	dc := gg.NewContextForRGBA(img)

	// 设置文本颜色
	dc.SetColor(fontColor)

	// 写入错误消息
	dc.DrawStringAnchored("无法截取网站截图", float64(width/2), float64(height/2), 0.5, 0.5)
	dc.DrawStringAnchored("Screenshot Failed", float64(width/2), float64(height/2)+40, 0.5, 0.5)

	// 保存图片
	f, err := os.Create(errorPath)
	if err != nil {
		return fmt.Errorf("创建错误图片文件失败: %v", err)
	}
	defer f.Close()

	if err := png.Encode(f, dc.Image()); err != nil {
		return fmt.Errorf("编码错误图片失败: %v", err)
	}

	return nil
}
