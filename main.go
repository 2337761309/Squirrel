package main

import (
	"bufio"
	"context"
	"flag"
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
	"sync/atomic"
	"time"

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
func checkDomain(domain string, config Config, resultChan chan<- Result, screenshotPool *ScreenshotPool) {
	// 如果已经指定了协议，直接使用
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		checkSingleDomain(domain, config, resultChan, screenshotPool)
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
		Timeout:   time.Duration(config.Timeout) * time.Second,
		Transport: transport,
	}

	// 处理重定向
	if !config.FollowRedirects {
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
		if config.ExtractInfo && resp.StatusCode < 400 {
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				pageContent := string(body)
				httpsResult.PageInfo = detectPageType(pageContent)
				httpsResult.Title = extractTitle(pageContent)
			}
		}

		// 如果需要截图，使用截图工作池
		if (config.Screenshot || (config.ScreenshotAlive && httpsResult.Alive)) && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(httpsDomain)

			// 确保截图目录存在
			if err := os.MkdirAll(config.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(httpsDomain, screenFilename, config.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				httpsResult.Screenshot = filepath.Join(config.ScreenshotDir, screenFilename)
			}
		}

		resultChan <- httpsResult
		return
	} else {
		// HTTPS请求出错
		httpsResult.Message = err.Error()
		httpsResult.StatusText = "无法访问"

		// 即使HTTPS请求出错，如果启用了截图功能，也使用截图工作池
		if config.Screenshot && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(httpsDomain)

			// 确保截图目录存在
			if err := os.MkdirAll(config.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(httpsDomain, screenFilename, config.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				httpsResult.Screenshot = filepath.Join(config.ScreenshotDir, screenFilename)
			}
		}
	}

	// HTTPS失败，尝试HTTP
	httpDomain := "http://" + domain
	checkSingleDomain(httpDomain, config, resultChan, screenshotPool)
}

// 使用指定协议检查单个域名
func checkSingleDomain(domain string, config Config, resultChan chan<- Result, screenshotPool *ScreenshotPool) {
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
		Timeout:   time.Duration(config.Timeout) * time.Second,
		Transport: transport,
	}

	// 处理重定向
	if !config.FollowRedirects {
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
		if config.Screenshot && screenshotPool != nil {
			// 为网站生成唯一的截图文件名
			screenFilename := generateScreenshotFilename(domain)

			// 确保截图目录存在
			if err := os.MkdirAll(config.ScreenshotDir, 0755); err == nil {
				// 提交截图任务到工作池
				resultCh := screenshotPool.Submit(domain, screenFilename, config.ScreenshotDir)

				// 不等待结果，先发送域名检测结果
				go func() {
					_ = <-resultCh // 使用下划线忽略返回值
				}()

				// 预设截图路径
				result.Screenshot = filepath.Join(config.ScreenshotDir, screenFilename)
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
	if config.ExtractInfo && resp.StatusCode < 400 {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			pageContent := string(body)
			result.PageInfo = detectPageType(pageContent)
			result.Title = extractTitle(pageContent)
		}
	}

	// 如果需要截图，使用截图工作池
	if (config.Screenshot || (config.ScreenshotAlive && result.Alive)) && screenshotPool != nil {
		// 为网站生成唯一的截图文件名
		screenFilename := generateScreenshotFilename(domain)

		// 确保截图目录存在
		if err := os.MkdirAll(config.ScreenshotDir, 0755); err == nil {
			// 提交截图任务到工作池
			resultCh := screenshotPool.Submit(domain, screenFilename, config.ScreenshotDir)

			// 不等待结果，先发送域名检测结果
			go func() {
				_ = <-resultCh // 使用下划线忽略返回值
			}()

			// 预设截图路径
			result.Screenshot = filepath.Join(config.ScreenshotDir, screenFilename)
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

func main() {
	fmt.Println(`
                               /$$                             /$$
                              |__/                            | $$
  /$$$$$$$  /$$$$$$  /$$   /$$ /$$  /$$$$$$  /$$$$$$  /$$$$$$ | $$
 /$$_____/ /$$__  $$| $$  | $$| $$ /$$__  $$/$$__  $$/$$__  $$| $$
|  $$$$$$ | $$  \ $$| $$  | $$| $$| $$  \__/ $$  \__/ $$$$$$$$| $$
 \____  $$| $$  | $$| $$  | $$| $$| $$     | $$     | $$_____/| $$
 /$$$$$$$/|  $$$$$$$|  $$$$$$/| $$| $$     | $$     |  $$$$$$$| $$
|_______/  \____  $$ \______/ |__/|__/     |__/      \_______/|__/
                | $$                                              
                | $$                                              
                |__/                                              
                
                    松鼠子域名检测工具 v1.2
`)

	// 解析命令行参数
	config := Config{}

	flag.IntVar(&config.Timeout, "timeout", 10, "请求超时时间(秒)")
	flag.IntVar(&config.Concurrency, "concurrency", 10, "并发数量")
	flag.BoolVar(&config.Verbose, "verbose", false, "显示详细输出")
	flag.BoolVar(&config.FollowRedirects, "follow", false, "跟随重定向")
	flag.BoolVar(&config.ShowResponseTime, "time", false, "显示响应时间")
	flag.StringVar(&config.OutputFile, "output", "", "输出结果到CSV文件")
	flag.StringVar(&config.ExcelFile, "excel", "", "输出结果到Excel文件")
	flag.BoolVar(&config.ExtractInfo, "extract", false, "提取页面重要信息（登录页面等）")
	flag.BoolVar(&config.OnlyAlive, "only-alive", false, "只导出存活的域名")
	flag.BoolVar(&config.Screenshot, "screenshot", false, "对所有网页进行截图")
	flag.BoolVar(&config.ScreenshotAlive, "screenshot-alive", false, "只截图存活的网页")
	flag.StringVar(&config.ScreenshotDir, "screenshot-dir", "screenshots", "截图保存目录")

	// HTML输出选项
	var htmlOutput, simpleHTML string
	flag.StringVar(&htmlOutput, "html", "", "输出结果到HTML文件")
	flag.StringVar(&simpleHTML, "simple-html", "", "输出结果到简化版HTML文件")

	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("用法: subdomain-checker [选项] <域名列表文件或逗号分隔的域名列表>")
		fmt.Println("\n选项:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// 如果启用任一截图功能，必须指定Excel文件或HTML文件
	if (config.Screenshot || config.ScreenshotAlive) && config.ExcelFile == "" && htmlOutput == "" && simpleHTML == "" {
		fmt.Println("错误: 启用截图功能时必须指定 -excel、-html 或 -simple-html 选项")
		os.Exit(1)
	}

	var domains []string
	var err error

	arg := flag.Arg(0)
	if strings.Contains(arg, ",") {
		// 从命令行参数中解析域名列表
		domains = strings.Split(arg, ",")
	} else {
		// 从文件中读取域名列表
		domains, err = readDomainsFromFile(arg)
		if err != nil {
			fmt.Printf("无法读取文件: %s\n", err)
			os.Exit(1)
		}
	}

	if len(domains) == 0 {
		fmt.Println("没有找到需要检测的域名")
		os.Exit(1)
	}

	fmt.Printf("总共需要检测 %d 个域名，并发数: %d，超时: %d秒\n",
		len(domains), config.Concurrency, config.Timeout)

	startTime := time.Now()
	totalDomains := len(domains)

	// 通道设置 - 增加缓冲区大小以减少阻塞
	resultChan := make(chan Result, totalDomains*2) // 用于传递检测结果，增加缓冲区
	domainChan := make(chan string, totalDomains)   // 用于分发域名任务
	doneChan := make(chan struct{})                 // 用于通知进度显示goroutine结束
	progressDone := make(chan struct{})             // 用于等待进度显示goroutine结束
	var wg sync.WaitGroup

	// 初始化截图工作池（如果需要）
	var screenshotPool *ScreenshotPool
	if config.Screenshot || config.ScreenshotAlive {
		// 使用较少的工作者用于截图，因为它们很消耗资源
		screenshotWorkers := config.Concurrency / 2
		if screenshotWorkers < 1 {
			screenshotWorkers = 1
		}
		screenshotPool = NewScreenshotPool(screenshotWorkers)
		screenshotPool.Start()
		defer screenshotPool.Stop() // 确保程序结束时关闭截图工作池
	}

	// 添加进度显示
	var processed int32 = 0

	// 启动进度显示goroutine
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(500 * time.Millisecond) // 更新频率提高到0.5秒一次
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt32(&processed)
				if current >= int32(totalDomains) {
					return
				}
				percent := float64(current) / float64(totalDomains) * 100
				fmt.Printf("\r进度: %.2f%% (%d/%d) - 耗时: %.1fs",
					percent, current, totalDomains, time.Since(startTime).Seconds())
			case <-doneChan:
				return
			}
		}
	}()

	// 用于保存所有结果的切片
	var resultsMutex sync.Mutex
	allResults := make([]Result, 0, totalDomains)

	// 统计计数器（使用原子操作避免锁）
	var alive, dead int32
	var pageTypeCountMutex sync.Mutex
	var pageTypeCount = make(map[string]int)
	var screenshotCount int32 = 0

	// 使用批处理来减少锁竞争
	const batchSize = 10
	resultBatchChan := make(chan []Result, totalDomains/batchSize+1)

	// 启动批处理结果的goroutine
	go func() {
		for resultBatch := range resultBatchChan {
			resultsMutex.Lock()
			for _, result := range resultBatch {
				// 更新计数器
				if result.Alive {
					atomic.AddInt32(&alive, 1)
					// 统计页面类型
					if result.PageInfo != nil {
						pageTypeCountMutex.Lock()
						pageTypeCount[result.PageInfo.Type]++
						pageTypeCountMutex.Unlock()
					}
				} else {
					atomic.AddInt32(&dead, 1)
				}

				// 统计截图
				if result.Screenshot != "" {
					// 如果使用 ScreenshotAlive 参数，则只统计存活页面的截图
					if config.ScreenshotAlive {
						if result.Alive {
							atomic.AddInt32(&screenshotCount, 1)
						}
					} else if config.Screenshot {
						// 使用普通截图参数，统计所有截图
						atomic.AddInt32(&screenshotCount, 1)
					}
				}

				// 添加到结果列表
				allResults = append(allResults, result)
			}
			resultsMutex.Unlock()
		}
	}()

	// 启动消费者goroutine，负责处理结果
	go func() {
		var resultBatch []Result
		for result := range resultChan {
			// 更新进度
			atomic.AddInt32(&processed, 1)

			// 添加到批次
			resultBatch = append(resultBatch, result)

			// 当批次达到指定大小或者是最后一个结果时，发送批次
			if len(resultBatch) >= batchSize || atomic.LoadInt32(&processed) == int32(totalDomains) {
				resultBatchChan <- resultBatch
				resultBatch = nil // 重置批次
			}
		}

		// 确保发送最后一个不完整的批次
		if len(resultBatch) > 0 {
			resultBatchChan <- resultBatch
		}

		close(resultBatchChan)
		close(doneChan)
	}()

	// 启动工作池
	for i := 0; i < config.Concurrency; i++ {
		wg.Add(1)
		go func(workerId int) {
			defer wg.Done()

			for domain := range domainChan {
				checkDomain(domain, config, resultChan, screenshotPool)
			}
		}(i)
	}

	// 发送所有域名到工作池
	for _, domain := range domains {
		domainChan <- domain
	}
	close(domainChan)

	// 等待所有检测完成
	wg.Wait()
	close(resultChan)

	// 等待所有结果都已输出
	<-doneChan
	<-progressDone // 等待进度显示goroutine结束

	// 清除进度显示行
	fmt.Printf("\r%-80s\r", " ")

	totalTime := time.Since(startTime)

	// 打印表头
	fmt.Println("\n检测结果 (总结):")
	fmt.Println("----------------------------------------")

	// 输出总结
	fmt.Printf("总计: %d 个域名, %d 个存活, %d 个无法访问\n", len(domains), atomic.LoadInt32(&alive), atomic.LoadInt32(&dead))

	// 如果启用了页面信息提取，显示页面类型统计
	if config.ExtractInfo && len(pageTypeCount) > 0 {
		fmt.Println("页面类型统计:")
		pageTypeCountMutex.Lock()
		for pageType, count := range pageTypeCount {
			fmt.Printf("  %s: %d 个\n", pageType, count)
		}
		pageTypeCountMutex.Unlock()
	}

	// 显示截图统计
	if config.Screenshot || config.ScreenshotAlive {
		if config.ScreenshotAlive {
			fmt.Printf("成功截图存活网站: %d 个\n", atomic.LoadInt32(&screenshotCount))
		} else {
			fmt.Printf("成功截图: %d 个\n", atomic.LoadInt32(&screenshotCount))
		}
	}

	fmt.Printf("检测耗时: %.2f 秒\n", totalTime.Seconds())

	// 保存结果到文件
	if config.OutputFile != "" {
		err := saveResultsToFile(allResults, config.OutputFile)
		if err != nil {
			fmt.Printf("保存结果到文件时出错: %s\n", err)
		} else {
			fmt.Printf("结果已保存到 %s\n", config.OutputFile)
		}
	}

	// 在程序结束时保存结果到Excel文件
	if config.ExcelFile != "" {
		err := saveResultsToExcel(allResults, config.ExcelFile, config.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到Excel文件时出错: %s\n", err)
		} else {
			fmt.Printf("结果已保存到 %s\n", config.ExcelFile)
		}
	}

	// 保存HTML报告
	if htmlOutput != "" {
		err := saveResultsToHTML(allResults, htmlOutput, config.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到HTML文件时出错: %s\n", err)
		} else {
			fmt.Printf("HTML报告已保存到 %s\n", htmlOutput)
		}
	}

	// 保存简化版HTML报告
	if simpleHTML != "" {
		err := saveResultsToSimpleHTML(allResults, simpleHTML, config.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到简化版HTML文件时出错: %s\n", err)
		} else {
			fmt.Printf("简化版HTML报告已保存到 %s\n", simpleHTML)
		}
	}
}

// 格式化截图状态
func formatScreenshotStatus(screenshotPath string) string {
	if screenshotPath == "" {
		return ""
	}
	if strings.Contains(screenshotPath, "error_") {
		return "[失败]"
	}
	return "[已截图]"
}

// 保存结果到HTML文件（简化版）
func saveResultsToSimpleHTML(results []Result, filename string, onlyAlive bool) error {
	// 创建HTML文件
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// 计算统计信息
	totalDomains := 0
	aliveDomains := 0
	for _, result := range results {
		// 如果只显示存活域名，跳过非存活的
		if onlyAlive && !result.Alive {
			continue
		}
		totalDomains++
		if result.Alive {
			aliveDomains++
		}
	}

	// 写入HTML头部
	html := `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>子域名检测结果</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 0; padding: 20px; background: #f5f5f5; }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { color: #333; text-align: center; margin-bottom: 30px; }
        .summary { background: #fff; padding: 15px; border-radius: 5px; margin-bottom: 20px; box-shadow: 0 2px 5px rgba(0,0,0,0.1); }
        .domain-card { background: #fff; margin-bottom: 20px; border-radius: 5px; overflow: hidden; box-shadow: 0 2px 5px rgba(0,0,0,0.1); }
        .domain-header { background: #f0f0f0; padding: 15px; cursor: pointer; }
        .domain-header h2 { margin: 0; font-size: 18px; }
        .domain-header a { color: #2056dd; text-decoration: none; transition: color 0.2s; }
        .domain-header a:hover { color: #1040aa; text-decoration: underline; }
        .domain-content { padding: 15px; }
        .domain-info { margin-bottom: 15px; }
        .domain-info span { font-weight: bold; }
        .status-alive { color: green; }
        .status-dead { color: red; }
        .screenshot-container { width: 100%; text-align: center; margin-top: 15px; }
        .screenshot-container h3 a { display: inline-block; padding: 8px 15px; background: #2056dd; color: white; text-decoration: none; border-radius: 4px; margin-bottom: 10px; transition: background 0.2s; }
        .screenshot-container h3 a:hover { background: #1040aa; }
        .screenshot { max-width: 100%; height: auto; border: 1px solid #ddd; }
        table { width: 100%; border-collapse: collapse; }
        th, td { padding: 10px; text-align: left; border-bottom: 1px solid #ddd; }
        th { background-color: #f2f2f2; }
        
        /* 导航菜单样式 */
        .nav-menu { 
            display: flex; 
            justify-content: center; 
            background: #fff; 
            padding: 15px; 
            border-radius: 5px; 
            margin-bottom: 20px; 
            box-shadow: 0 2px 5px rgba(0,0,0,0.1); 
        }
        .nav-item {
            margin: 0 15px;
            padding: 10px 20px;
            border-radius: 5px;
            cursor: pointer;
            font-weight: bold;
            transition: all 0.3s ease;
        }
        .nav-item:hover {
            background: #f0f0f0;
        }
        .nav-item.active {
            background: #2056dd;
            color: white;
        }
        .counter {
            display: inline-block;
            background: #eee;
            color: #333;
            border-radius: 50%;
            width: 24px;
            height: 24px;
            text-align: center;
            line-height: 24px;
            margin-left: 8px;
            font-size: 12px;
        }
        .nav-item.active .counter {
            background: #fff;
            color: #2056dd;
        }
        .hidden {
            display: none;
        }
        
        /* 搜索框样式 */
        .search-container {
            display: flex;
            justify-content: center;
            margin-bottom: 20px;
        }
        .search-box {
            width: 100%;
            max-width: 500px;
            padding: 10px 15px;
            border: 2px solid #ddd;
            border-radius: 5px;
            font-size: 16px;
            transition: border-color 0.3s;
        }
        .search-box:focus {
            border-color: #2056dd;
            outline: none;
        }
        .search-box::placeholder {
            color: #aaa;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>子域名检测结果</h1>
        <div class="summary">
`

	// 添加总结信息
	html += fmt.Sprintf(`
            <p>共检测 %d 个域名，其中 %d 个存活，%d 个无法访问</p>
            <p>报告生成时间：%s</p>
        </div>
        
        <!-- 导航菜单 -->
        <div class="nav-menu">
            <div class="nav-item active" data-filter="all">全部<span class="counter">%d</span></div>
            <div class="nav-item" data-filter="alive">存活<span class="counter">%d</span></div>
            <div class="nav-item" data-filter="dead">不存活<span class="counter">%d</span></div>
        </div>
        
        <!-- 搜索框 -->
        <div class="search-container">
            <input type="text" class="search-box" placeholder="输入域名关键词或状态码(如200、404等)进行搜索..." id="domainSearch">
            <p style="text-align: center; color: #666; margin-top: 5px; font-size: 12px;">支持搜索：域名、状态码(如200、404)、状态文本(如存活、禁止访问)</p>
        </div>
`, totalDomains, aliveDomains, totalDomains-aliveDomains, time.Now().Format("2006-01-02 15:04:05"),
		totalDomains, aliveDomains, totalDomains-aliveDomains)

	// 添加域名卡片
	for _, result := range results {
		// 如果只显示存活域名，跳过非存活的
		if onlyAlive && !result.Alive {
			continue
		}

		statusClass := "status-dead"
		domainStatus := "dead"
		if result.Alive {
			statusClass = "status-alive"
			domainStatus = "alive"
		}

		// 页面类型
		pageType := "-"
		if result.PageInfo != nil {
			pageType = result.PageInfo.Type
		}

		// 确保域名链接包含协议
		domainLink := result.Domain
		if !strings.HasPrefix(domainLink, "http://") && !strings.HasPrefix(domainLink, "https://") {
			// 如果结果是存活状态，优先使用https
			if result.Alive {
				domainLink = "https://" + domainLink
			} else {
				domainLink = "http://" + domainLink
			}
		}

		html += fmt.Sprintf(`
        <div class="domain-card domain-%s">
            <div class="domain-header">
                <h2><a href="%s" target="_blank" rel="noopener noreferrer">%s</a></h2>
            </div>
            <div class="domain-content">
                <div class="domain-info">
                    <p><span>状态:</span> <span class="%s">%s</span></p>
                    <p><span>状态码:</span> %d</p>
                    <p><span>响应时间:</span> %.2f ms</p>
                    <p><span>页面类型:</span> %s</p>
                    <p><span>页面标题:</span> %s</p>
                    <p><span>消息:</span> %s</p>
                </div>
`, domainStatus, domainLink, result.Domain, statusClass, result.StatusText, result.Status, result.ResponseTime.Seconds()*1000, pageType, result.Title, result.Message)

		// 如果有截图，添加截图区域
		if result.Screenshot != "" {
			// 使用相对路径
			relativeScreenshotPath := filepath.Base(result.Screenshot)
			html += fmt.Sprintf(`
                <div class="screenshot-container">
                    <h3><a href="%s" target="_blank" rel="noopener noreferrer">访问网站</a></h3>
                    <img class="screenshot" src="screenshots/%s" alt="%s 的截图">
                </div>
`, domainLink, relativeScreenshotPath, result.Domain)
		}

		html += `
            </div>
        </div>
`
	}

	// 添加JS脚本
	html += `
    </div>
    
    <!-- JavaScript脚本用于域名过滤 -->
    <script>
        document.addEventListener('DOMContentLoaded', function() {
            // 获取所有导航项和域名卡片
            const navItems = document.querySelectorAll('.nav-item');
            const domainCards = document.querySelectorAll('.domain-card');
            const searchBox = document.getElementById('domainSearch');
            
            // 当前过滤类型
            let currentFilter = 'all';
            
            // 为导航项添加点击事件
            navItems.forEach(item => {
                item.addEventListener('click', function() {
                    // 移除所有导航项的active类
                    navItems.forEach(nav => nav.classList.remove('active'));
                    
                    // 为当前点击的导航项添加active类
                    this.classList.add('active');
                    
                    // 获取过滤条件
                    currentFilter = this.getAttribute('data-filter');
                    
                    // 应用过滤和搜索
                    applyFilters();
                });
            });
            
            // 为搜索框添加输入事件
            searchBox.addEventListener('input', function() {
                applyFilters();
            });
            
            // 应用过滤和搜索
            function applyFilters() {
                const searchTerm = searchBox.value.toLowerCase();
                
                domainCards.forEach(card => {
                    const domainText = card.querySelector('h2').textContent.toLowerCase();
                    const statusCode = card.querySelector('.domain-info p:nth-child(2)').textContent.toLowerCase();
                    const statusText = card.querySelector('.domain-info p:nth-child(1)').textContent.toLowerCase();
                    
                    // 检查域名、状态码或状态文本是否匹配搜索词
                    const matchesSearch = searchTerm === '' || 
                                         domainText.includes(searchTerm) || 
                                         statusCode.includes(searchTerm) ||
                                         statusText.includes(searchTerm);
                    
                    // 检查是否匹配当前过滤条件
                    let matchesFilter = true;
                    if (currentFilter === 'alive') {
                        matchesFilter = card.classList.contains('domain-alive');
                    } else if (currentFilter === 'dead') {
                        matchesFilter = card.classList.contains('domain-dead');
                    }
                    
                    // 同时满足搜索和过滤条件才显示
                    if (matchesSearch && matchesFilter) {
                        card.classList.remove('hidden');
                    } else {
                        card.classList.add('hidden');
                    }
                });
            }
        });
    </script>
</body>
</html>
`

	// 写入HTML内容到文件
	_, err = file.WriteString(html)
	return err
}

// 保存结果到HTML文件（带详细信息）
func saveResultsToHTML(results []Result, filename string, onlyAlive bool) error {
	return saveResultsToSimpleHTML(results, filename, onlyAlive)
}
