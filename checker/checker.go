package checker

import (
	"bufio"
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
	"github.com/xuri/excelize/v2"
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
		if cfg.ExtractInfo && resp.StatusCode < 400 {
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				pageContent := string(body)
				httpsResult.PageInfo = detectPageType(pageContent)
				httpsResult.Title = extractTitle(pageContent)
			}
		}

		// 如果需要截图，使用截图工作池
		if (cfg.Screenshot || (cfg.ScreenshotAlive && httpsResult.Alive)) && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(httpsDomain)

			// 确保截图目录存在
			if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(httpsDomain, screenFilename, cfg.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				httpsResult.Screenshot = filepath.Join(cfg.ScreenshotDir, screenFilename)
			}
		}

		resultChan <- httpsResult
		return
	} else {
		// HTTPS请求出错
		httpsResult.Message = err.Error()
		httpsResult.StatusText = "无法访问"

		// 即使HTTPS请求出错，如果启用了截图功能，也使用截图工作池
		if cfg.Screenshot && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(httpsDomain)

			// 确保截图目录存在
			if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(httpsDomain, screenFilename, cfg.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				httpsResult.Screenshot = filepath.Join(cfg.ScreenshotDir, screenFilename)
			}
		}
	}

	// HTTPS失败，尝试HTTP
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

		// 即使请求出错，如果启用了截图功能，也使用截图工作池
		if cfg.Screenshot && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(domain)

			// 确保截图目录存在
			if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(domain, screenFilename, cfg.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				result.Screenshot = filepath.Join(cfg.ScreenshotDir, screenFilename)
			}
		}

		resultChan <- result
		return
	}
	defer resp.Body.Close()

	result.Status = resp.StatusCode

	// 根据状态码设置状态文本和存活标志
	result.StatusText, result.Alive = getStatusTextAndAlive(resp.StatusCode)
	result.Message = http.StatusText(resp.StatusCode)

	// 提取页面信息
	if cfg.ExtractInfo && resp.StatusCode < 400 {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			pageContent := string(body)
			result.PageInfo = detectPageType(pageContent)
			result.Title = extractTitle(pageContent)
		}
	}

	// 如果需要截图，使用截图工作池
	if (cfg.Screenshot || (cfg.ScreenshotAlive && result.Alive)) && screenshotPool != nil {
		// 为网站生成唯一的截图文件名
		screenFilename := generateScreenshotFilename(domain)

		// 确保截图目录存在
		if err := os.MkdirAll(cfg.ScreenshotDir, 0755); err == nil {
			// 提交截图任务到工作池
			resultCh := screenshotPool.Submit(domain, screenFilename, cfg.ScreenshotDir)

			// 不等待结果，先发送域名检测结果
			go func() {
				_ = <-resultCh // 使用下划线忽略返回值
			}()

			// 预设截图路径
			result.Screenshot = filepath.Join(cfg.ScreenshotDir, screenFilename)
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

// 从文件中读取域名
func readDomainsFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var domains []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		domain := strings.TrimSpace(scanner.Text())
		if domain != "" && !strings.HasPrefix(domain, "#") {
			domains = append(domains, domain)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return domains, nil
}

// 保存结果到文件
func saveResultsToFile(results []Result, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// 写入标题行
	fmt.Fprintf(file, "域名,状态,状态码,响应时间(毫秒),页面类型,页面标题,消息\n")

	// 写入数据行
	for _, result := range results {
		pageType := ""
		if result.PageInfo != nil {
			pageType = result.PageInfo.Type
		}

		fmt.Fprintf(file, "%s,%s,%d,%.2f,%s,%s,%s\n",
			result.Domain,
			result.StatusText,
			result.Status,
			float64(result.ResponseTime.Milliseconds()),
			pageType,
			strings.ReplaceAll(result.Title, ",", " "),   // 避免标题中的逗号影响CSV格式
			strings.ReplaceAll(result.Message, ",", " ")) // 避免消息中的逗号影响CSV格式
	}

	return nil
}

// 保存结果到 Excel 文件
func saveResultsToExcel(results []Result, filename string, onlyAlive bool) error {
	// 创建输出目录（如果不存在）
	outputDir := filepath.Dir(filename)
	if outputDir != "" && outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("创建输出目录失败: %v", err)
		}
	}

	// 创建一个新的 Excel 文件
	f := excelize.NewFile()
	defer func() {
		if err := f.Close(); err != nil {
			fmt.Printf("关闭 Excel 文件时出错: %s\n", err)
		}
	}()

	// 设置表头
	sheetName := "子域名检测结果"
	f.SetSheetName("Sheet1", sheetName)
	headers := []string{"域名", "状态", "状态码", "响应时间(毫秒)", "页面类型", "页面标题", "消息", "截图"}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheetName, cell, header)
	}

	// 创建截图工作表
	screenshotSheet := "页面截图"
	f.NewSheet(screenshotSheet)
	f.SetCellValue(screenshotSheet, "A1", "域名")
	f.SetCellValue(screenshotSheet, "B1", "截图")

	// 设置表头样式
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{
			Type:    "pattern",
			Color:   []string{"#D9D9D9"},
			Pattern: 1,
		},
		Alignment: &excelize.Alignment{
			Horizontal: "center",
			Vertical:   "center",
		},
		Border: []excelize.Border{
			{Type: "left", Color: "#000000", Style: 1},
			{Type: "right", Color: "#000000", Style: 1},
			{Type: "top", Color: "#000000", Style: 1},
			{Type: "bottom", Color: "#000000", Style: 1},
		},
	})
	f.SetCellStyle(sheetName, "A1", fmt.Sprintf("%c1", 'A'+len(headers)-1), headerStyle)
	f.SetCellStyle(screenshotSheet, "A1", "B1", headerStyle)

	// 写入数据行
	row := 2           // 从第二行开始
	screenshotRow := 2 // 截图表从第二行开始
	for _, result := range results {
		// 如果只导出存活的域名，则跳过非存活的
		if onlyAlive && !result.Alive {
			continue
		}

		pageType := ""
		if result.PageInfo != nil {
			pageType = result.PageInfo.Type
		}

		// 设置单元格样式
		contentStyle, _ := f.NewStyle(&excelize.Style{
			Border: []excelize.Border{
				{Type: "left", Color: "#000000", Style: 1},
				{Type: "right", Color: "#000000", Style: 1},
				{Type: "top", Color: "#000000", Style: 1},
				{Type: "bottom", Color: "#000000", Style: 1},
			},
		})

		// 写入一行数据到主表
		f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), result.Domain)
		f.SetCellValue(sheetName, fmt.Sprintf("B%d", row), result.StatusText)
		f.SetCellValue(sheetName, fmt.Sprintf("C%d", row), result.Status)
		f.SetCellValue(sheetName, fmt.Sprintf("D%d", row), float64(result.ResponseTime.Milliseconds()))
		f.SetCellValue(sheetName, fmt.Sprintf("E%d", row), pageType)
		f.SetCellValue(sheetName, fmt.Sprintf("F%d", row), result.Title)
		f.SetCellValue(sheetName, fmt.Sprintf("G%d", row), result.Message)

		// 应用内容样式
		f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), fmt.Sprintf("G%d", row), contentStyle)

		// 处理截图
		if result.Screenshot != "" {
			// 在主表中添加"查看截图"超链接
			f.SetCellValue(sheetName, fmt.Sprintf("H%d", row), "查看截图")
			linkStyle, _ := f.NewStyle(&excelize.Style{
				Font: &excelize.Font{
					Color:     "#0563C1",
					Underline: "single",
				},
				Border: []excelize.Border{
					{Type: "left", Color: "#000000", Style: 1},
					{Type: "right", Color: "#000000", Style: 1},
					{Type: "top", Color: "#000000", Style: 1},
					{Type: "bottom", Color: "#000000", Style: 1},
				},
				Alignment: &excelize.Alignment{
					Horizontal: "center",
					Vertical:   "center",
				},
			})
			f.SetCellStyle(sheetName, fmt.Sprintf("H%d", row), fmt.Sprintf("H%d", row), linkStyle)
			// 设置超链接到截图工作表的对应行
			f.SetCellHyperLink(sheetName, fmt.Sprintf("H%d", row), fmt.Sprintf("'%s'!A%d", screenshotSheet, screenshotRow), "Location")

			// 在截图表中添加域名和截图
			f.SetCellValue(screenshotSheet, fmt.Sprintf("A%d", screenshotRow), result.Domain)

			// 如果文件存在，添加图片
			if _, err := os.Stat(result.Screenshot); err == nil {
				// 设置行高以适应图片
				f.SetRowHeight(screenshotSheet, screenshotRow, 300)
				// 添加图片
				if err := f.AddPicture(screenshotSheet, fmt.Sprintf("B%d", screenshotRow), result.Screenshot, &excelize.GraphicOptions{
					ScaleX:          0.3,  // 将图片缩小到30%（原来是10%）
					ScaleY:          0.3,  // 将图片缩小到30%（原来是10%）
					LockAspectRatio: true, // 锁定宽高比
					Positioning:     "oneCell",
				}); err != nil {
					fmt.Printf("添加图片到Excel时出错: %s\n", err)
				}
			} else {
				f.SetCellValue(screenshotSheet, fmt.Sprintf("B%d", screenshotRow), "无法获取截图")
			}

			// 设置单元格样式
			f.SetCellStyle(screenshotSheet, fmt.Sprintf("A%d", screenshotRow), fmt.Sprintf("A%d", screenshotRow), contentStyle)
			f.SetCellStyle(screenshotSheet, fmt.Sprintf("B%d", screenshotRow), fmt.Sprintf("B%d", screenshotRow), contentStyle)

			screenshotRow++
		} else {
			f.SetCellValue(sheetName, fmt.Sprintf("H%d", row), "无截图")
			f.SetCellStyle(sheetName, fmt.Sprintf("H%d", row), fmt.Sprintf("H%d", row), contentStyle)
		}

		row++
	}

	// 自动调整列宽
	for i := range headers {
		col, _ := excelize.ColumnNumberToName(i + 1)
		f.SetColWidth(sheetName, col, col, 20)
	}
	f.SetColWidth(screenshotSheet, "A", "A", 40)
	f.SetColWidth(screenshotSheet, "B", "B", 200) // 加宽截图列以便更好地显示截图（原来是150）

	// 冻结表头
	f.SetPanes(sheetName, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})
	f.SetPanes(screenshotSheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})

	// 保存文件
	if err := f.SaveAs(filename); err != nil {
		return err
	}

	return nil
}

// 为域名生成唯一的截图文件名
func generateScreenshotFilename(domain string) string {
	// 移除协议部分
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	// 替换不允许在文件名中使用的字符
	domain = strings.ReplaceAll(domain, "/", "_")
	domain = strings.ReplaceAll(domain, ":", "_")
	domain = strings.ReplaceAll(domain, "?", "_")
	domain = strings.ReplaceAll(domain, "&", "_")
	domain = strings.ReplaceAll(domain, "=", "_")
	domain = strings.ReplaceAll(domain, "*", "_")
	domain = strings.ReplaceAll(domain, "\"", "_")
	domain = strings.ReplaceAll(domain, "<", "_")
	domain = strings.ReplaceAll(domain, ">", "_")
	domain = strings.ReplaceAll(domain, "|", "_")

	// 生成时间戳后缀确保唯一性
	timestamp := time.Now().UnixNano()
	return fmt.Sprintf("%s_%d.png", domain, timestamp)
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

	// 设置字体 - 使用默认字体，不尝试加载自定义字体
	// gg库会自动使用可用的默认字体
	if err := dc.LoadFontFace("", 30); err != nil {
		// 如果加载字体失败，只记录错误，继续执行
		fmt.Printf("加载字体失败: %v，将使用简单文本\n", err)
	}

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

// 截断字符串到指定长度
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
