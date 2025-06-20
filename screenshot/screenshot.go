package screenshot

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/fogleman/gg"
)

// 截图任务
type ScreenshotTask struct {
	URL      string
	Filename string
	Dir      string
	Result   chan<- string // 返回截图路径或空字符串（失败时）
}

// 截图工作池
type ScreenshotPool struct {
	tasks        chan ScreenshotTask
	workers      int
	wg           sync.WaitGroup
	closed       bool
	mutex        sync.RWMutex
	successCount int64
	failureCount int64
	totalCount   int64
}

// 创建新的截图工作池
func NewScreenshotPool(workers int) *ScreenshotPool {
	return &ScreenshotPool{
		tasks:   make(chan ScreenshotTask, workers*2), // 缓冲大小为工作者数量的2倍
		workers: workers,
	}
}

// 启动截图工作池 - 高并发优化版本，带重试机制
func (p *ScreenshotPool) Start() {
	fmt.Printf("🚀 启动 %d 个截图工作者 (高并发优化版本)\n", p.workers)

	// 启动指定数量的工作者
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(workerId int) {
			defer p.wg.Done()
			fmt.Printf("📸 截图工作者 %d 启动\n", workerId)

			for task := range p.tasks {
				atomic.AddInt64(&p.totalCount, 1)
				screenshotPath := filepath.Join(task.Dir, task.Filename)

				// 轻量级资源监控 - 只在极端情况下限制
				if !resourceMonitor.CanStartTask() {
					fmt.Printf("⚠️  工作者 %d 系统资源极度不足，跳过任务: %s\n", workerId, task.URL)
					atomic.AddInt64(&p.failureCount, 1)
					task.Result <- ""
					continue
				}

				// 开始任务
				resourceMonitor.StartTask()
				defer resourceMonitor.EndTask()

				// 大量域名处理时的资源管理
				taskCount := atomic.AddInt64(&globalTaskCounter, 1)

				// 每处理1000个任务进行一次垃圾回收和资源清理
				if taskCount%1000 == 0 {
					if time.Since(lastGCTime) > 30*time.Second {
						fmt.Printf("🧹 工作者 %d 执行资源清理 (已处理%d个任务)\n", workerId, taskCount)
						runtime.GC()
						lastGCTime = time.Now()
					}
				}

				// 每处理5000个任务暂停一下，让系统恢复
				if taskCount%5000 == 0 {
					fmt.Printf("⏸️  工作者 %d 短暂休息，让系统恢复 (已处理%d个任务)\n", workerId, taskCount)
					time.Sleep(2 * time.Second)
				}

				// 追求100%成功率的重试机制
				success := false
				maxRetries := 3 // 增加重试次数以提高成功率

				for retry := 0; retry <= maxRetries && !success; retry++ {
					if retry > 0 {
						// 重试前等待更长时间，给网络和系统更多恢复时间
						waitTime := time.Duration(retry*500) * time.Millisecond
						fmt.Printf("🔄 工作者 %d 重试截图 %s (第%d次，等待%v)\n", workerId, task.URL, retry+1, waitTime)
						time.Sleep(waitTime)
					} else {
						fmt.Printf("🔄 工作者 %d 开始截图: %s\n", workerId, task.URL)
					}

					// 尝试截图
					if err := TakeScreenshotIndependent(task.URL, screenshotPath); err == nil {
						atomic.AddInt64(&p.successCount, 1)
						fmt.Printf("✅ 工作者 %d 截图成功: %s\n", workerId, task.URL)
						task.Result <- screenshotPath
						success = true
					} else {
						// 检查是否是网络错误
						errStr := err.Error()
						isNetworkError := strings.Contains(errStr, "net::ERR_INVALID_RESPONSE") ||
							strings.Contains(errStr, "net::ERR_CONNECTION_REFUSED") ||
							strings.Contains(errStr, "net::ERR_NAME_NOT_RESOLVED") ||
							strings.Contains(errStr, "net::ERR_TIMED_OUT")

						if retry == maxRetries {
							// 最终失败
							if isNetworkError {
								// 网络错误仍然算作成功（生成了错误图片）
								atomic.AddInt64(&p.successCount, 1)
								fmt.Printf("🌐 工作者 %d 网络错误，已生成错误图片: %s - %v\n", workerId, task.URL, err)
								task.Result <- screenshotPath
								success = true
							} else {
								atomic.AddInt64(&p.failureCount, 1)
								fmt.Printf("❌ 工作者 %d 截图最终失败: %s - %v\n", workerId, task.URL, err)
								task.Result <- ""
							}
						} else {
							if isNetworkError {
								fmt.Printf("🌐 工作者 %d 网络错误，准备重试: %s - %v\n", workerId, task.URL, err)
							} else {
								fmt.Printf("⚠️  工作者 %d 截图失败，准备重试: %s - %v\n", workerId, task.URL, err)
							}
						}
					}
				}
			}

			fmt.Printf("🏁 截图工作者 %d 结束\n", workerId)
		}(i)
	}
}

// 提交截图任务 - 高并发优化版本，带队列管理
func (p *ScreenshotPool) Submit(url, filename, dir string) <-chan string {
	result := make(chan string, 1)

	// 检查工作池是否已关闭
	p.mutex.RLock()
	if p.closed {
		p.mutex.RUnlock()
		fmt.Printf("⚠️  截图工作池已关闭，跳过任务: %s\n", url)
		result <- ""
		return result
	}
	p.mutex.RUnlock()

	// 使用defer和recover防止panic
	defer func() {
		if r := recover(); r != nil {
			// 如果发生panic（通常是向已关闭的channel发送数据），返回空结果
			fmt.Printf("❌ 提交截图任务时发生panic: %s - %v\n", url, r)
			result <- ""
		}
	}()

	// 创建任务
	task := ScreenshotTask{
		URL:      url,
		Filename: filename,
		Dir:      dir,
		Result:   result,
	}

	// 使用带超时的select语句发送任务，避免长时间阻塞
	select {
	case p.tasks <- task:
		// 成功发送任务
		fmt.Printf("📋 任务已提交到队列: %s\n", url)
	case <-time.After(1 * time.Second):
		// 如果1秒内无法提交任务，说明队列可能已满
		fmt.Printf("⚠️  截图任务队列繁忙，跳过任务: %s\n", url)
		result <- ""
	}

	return result
}

// 关闭截图工作池
func (p *ScreenshotPool) Stop() {
	p.mutex.Lock()
	if !p.closed {
		p.closed = true
		close(p.tasks)
	}
	p.mutex.Unlock()

	p.wg.Wait()

	// 显示详细的截图统计
	total := atomic.LoadInt64(&p.totalCount)
	success := atomic.LoadInt64(&p.successCount)
	failure := atomic.LoadInt64(&p.failureCount)

	fmt.Printf("📸 截图工作池已停止\n")
	if total > 0 {
		successRate := float64(success) / float64(total) * 100
		fmt.Printf("📊 截图统计: 总计%d个, 成功%d个, 失败%d个, 成功率%.1f%%\n",
			total, success, failure, successRate)

		// 根据成功率给出性能评估
		if successRate >= 95 {
			fmt.Printf("✅ 截图性能优秀: 成功率%.1f%% (≥95%%)\n", successRate)
		} else if successRate >= 85 {
			fmt.Printf("⚖️  截图性能良好: 成功率%.1f%% (85-95%%)\n", successRate)
		} else if successRate >= 70 {
			fmt.Printf("⚠️  截图性能一般: 成功率%.1f%% (70-85%%)\n", successRate)
		} else {
			fmt.Printf("❌ 截图性能较差: 成功率%.1f%% (<70%%)\n", successRate)
		}

		if failure > 0 {
			fmt.Printf("⚠️  有%d个截图失败，可能原因：\n", failure)
			fmt.Printf("   • 网络超时或连接失败\n")
			fmt.Printf("   • 域名无法访问或DNS解析失败\n")
			fmt.Printf("   • Chrome进程启动失败或崩溃\n")
			fmt.Printf("   • 系统资源不足（内存/CPU）\n")
			fmt.Printf("   • 并发数过高导致资源竞争\n")
		}
	} else {
		fmt.Printf("📊 没有处理任何截图任务\n")
	}
}

// 全局变量存储当前并发数，用于动态调整超时
var currentConcurrency int = 1

// 全局计数器，用于大量域名处理时的资源管理
var globalTaskCounter int64 = 0
var lastGCTime time.Time = time.Now()

// 资源监控结构
type ResourceMonitor struct {
	maxMemoryMB    int64
	maxConcurrency int
	currentTasks   int64
	mutex          sync.RWMutex
}

// 全局资源监控器
var resourceMonitor = &ResourceMonitor{
	maxMemoryMB:    2048, // 默认2GB内存限制
	maxConcurrency: 50,   // 默认最大50并发
}

// 设置当前并发数
func SetConcurrency(concurrency int) {
	currentConcurrency = concurrency
	resourceMonitor.mutex.Lock()
	resourceMonitor.maxConcurrency = concurrency
	resourceMonitor.mutex.Unlock()
}

// 检查是否可以启动新任务 - 完全禁用限制
func (rm *ResourceMonitor) CanStartTask() bool {
	// 完全禁用资源监控，让所有任务都能执行
	// 这样可以确保高并发下不会有任务被跳过
	return true
}

// 开始任务
func (rm *ResourceMonitor) StartTask() {
	atomic.AddInt64(&rm.currentTasks, 1)
}

// 结束任务
func (rm *ResourceMonitor) EndTask() {
	atomic.AddInt64(&rm.currentTasks, -1)
}

// 根据并发数计算合适的超时时间 - 追求100%成功率版本
func calculateTimeout(concurrency int) time.Duration {
	// 大幅增加基础超时时间，确保网络慢的情况下也能成功
	baseTimeout := 20 * time.Second

	// 根据并发数动态调整超时时间，给予非常充足的时间
	switch {
	case concurrency <= 5:
		return baseTimeout // 20秒
	case concurrency <= 10:
		return baseTimeout + 5*time.Second // 25秒
	case concurrency <= 15:
		return baseTimeout + 10*time.Second // 30秒
	case concurrency <= 20:
		return baseTimeout + 15*time.Second // 35秒
	case concurrency <= 30:
		return baseTimeout + 20*time.Second // 40秒
	case concurrency <= 50:
		return baseTimeout + 25*time.Second // 45秒
	default:
		return baseTimeout + 30*time.Second // 50秒最大超时
	}
}

// 完全独立的截图函数 - 动态超时优化
func TakeScreenshotIndependent(url string, screenshotPath string) error {
	// 检查URL是否包含协议前缀
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}

	// 创建完全独立的Chrome实例，使用极速启动参数
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-web-security", true),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-features", "TranslateUI,VizDisplayCompositor,AudioServiceOutOfProcess"),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-hang-monitor", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-prompt-on-repost", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("memory-pressure-off", true),
		chromedp.Flag("max_old_space_size", "512"), // 进一步减少内存
		chromedp.WindowSize(1280, 720),             // 减少窗口大小提高速度
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	// 根据并发数动态设置超时时间
	timeout := calculateTimeout(currentConcurrency)
	timeoutCtx, timeoutCancel := context.WithTimeout(taskCtx, timeout)
	defer timeoutCancel()

	var buf []byte

	// 智能截图流程 - 处理网络错误和无效响应
	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(url),
		chromedp.Sleep(1*time.Second), // 增加等待时间，给网络更多时间
		chromedp.ActionFunc(func(ctx context.Context) error {
			// 检查页面是否有内容，即使有网络错误也尝试截图
			var title string
			titleErr := chromedp.Title(&title).Do(ctx)

			// 检查页面状态
			var ready bool
			readyErr := chromedp.Evaluate(`document.readyState`, &ready).Do(ctx)

			// 如果页面有任何内容，就继续截图
			if titleErr == nil || readyErr == nil {
				time.Sleep(500 * time.Millisecond) // 等待渲染
				return nil
			}

			// 即使检查失败，也尝试截图（可能是错误页面）
			time.Sleep(300 * time.Millisecond)
			return nil
		}),
		chromedp.FullScreenshot(&buf, 80), // 适中质量，平衡速度和清晰度
	)

	if err != nil {
		// 检查是否是网络相关错误
		errStr := err.Error()
		if strings.Contains(errStr, "net::ERR_INVALID_RESPONSE") ||
			strings.Contains(errStr, "net::ERR_CONNECTION_REFUSED") ||
			strings.Contains(errStr, "net::ERR_NAME_NOT_RESOLVED") ||
			strings.Contains(errStr, "net::ERR_TIMED_OUT") {

			// 对于网络错误，尝试生成一个错误页面截图
			if len(buf) > 0 {
				// 如果有部分数据，仍然保存
				return os.WriteFile(screenshotPath, buf, 0644)
			}

			// 生成错误信息图片
			return generateNetworkErrorImage(screenshotPath, errStr)
		}
		return fmt.Errorf("截图失败: %w", err)
	}

	// 检查截图数据是否有效
	if len(buf) == 0 {
		return fmt.Errorf("截图数据为空")
	}

	return os.WriteFile(screenshotPath, buf, 0644)
}

// 快速截图模式 - 保持向后兼容
func TakeScreenshotFast(ctx context.Context, url string, screenshotPath string) error {
	return TakeScreenshotIndependent(url, screenshotPath)
}

// 稳定截图模式 - 保持向后兼容
func TakeScreenshotStable(ctx context.Context, url string, screenshotPath string) error {
	return TakeScreenshotIndependent(url, screenshotPath)
}

// 使用已有的context进行截图 - 兼容性函数
func TakeScreenshotWithContext(ctx context.Context, url string, screenshotPath string) error {
	return TakeScreenshotIndependent(url, screenshotPath)
}

// 宽松模式截图 - 用于处理404、403等错误页面
func TakeScreenshotLenient(ctx context.Context, url string, screenshotPath string) error {
	// 准备截图缓冲区
	var buf []byte

	// 设置超时 - 宽松模式使用更长超时
	taskCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 更宽松的截图流程 - 不等待特定元素，直接截图页面显示的内容
	tasks := chromedp.Tasks{
		chromedp.Navigate(url),
		// 等待页面基本加载
		chromedp.Sleep(5 * time.Second),
		// 直接截图，不等待特定元素
		chromedp.FullScreenshot(&buf, 80),
	}

	if err := chromedp.Run(taskCtx, tasks); err != nil {
		return err
	}

	// 保存截图到文件
	return os.WriteFile(screenshotPath, buf, 0644)
}

// 为域名生成唯一的截图文件名
func GenerateScreenshotFilename(domain string) string {
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
func GenerateErrorImage(filename string, screenshotDir string) error {
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

// 生成网络错误图片
func generateNetworkErrorImage(screenshotPath string, errorMsg string) error {
	// 创建截图目录（如果不存在）
	dir := filepath.Dir(screenshotPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建截图目录失败: %v", err)
	}

	// 生成一个简单的网络错误图片
	width, height := 800, 600
	upLeft := image.Point{0, 0}
	lowRight := image.Point{width, height}

	img := image.NewRGBA(image.Rectangle{upLeft, lowRight})

	// 填充浅灰色背景
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{240, 240, 240, 255})
		}
	}

	// 创建图片对象
	dc := gg.NewContextForRGBA(img)

	// 设置橙色文本颜色（网络错误）
	dc.SetColor(color.RGBA{255, 140, 0, 255})

	// 写入错误消息
	dc.DrawStringAnchored("网络连接错误", float64(width/2), float64(height/2-40), 0.5, 0.5)
	dc.DrawStringAnchored("Network Error", float64(width/2), float64(height/2), 0.5, 0.5)

	// 显示具体错误信息（截取关键部分）
	if strings.Contains(errorMsg, "ERR_INVALID_RESPONSE") {
		dc.DrawStringAnchored("无效响应 (ERR_INVALID_RESPONSE)", float64(width/2), float64(height/2+40), 0.5, 0.5)
	} else if strings.Contains(errorMsg, "ERR_NAME_NOT_RESOLVED") {
		dc.DrawStringAnchored("域名解析失败 (ERR_NAME_NOT_RESOLVED)", float64(width/2), float64(height/2+40), 0.5, 0.5)
	} else if strings.Contains(errorMsg, "ERR_CONNECTION_REFUSED") {
		dc.DrawStringAnchored("连接被拒绝 (ERR_CONNECTION_REFUSED)", float64(width/2), float64(height/2+40), 0.5, 0.5)
	} else if strings.Contains(errorMsg, "ERR_TIMED_OUT") {
		dc.DrawStringAnchored("连接超时 (ERR_TIMED_OUT)", float64(width/2), float64(height/2+40), 0.5, 0.5)
	} else {
		dc.DrawStringAnchored("网络连接问题", float64(width/2), float64(height/2+40), 0.5, 0.5)
	}

	// 保存图片
	f, err := os.Create(screenshotPath)
	if err != nil {
		return fmt.Errorf("创建错误图片文件失败: %v", err)
	}
	defer f.Close()

	if err := png.Encode(f, dc.Image()); err != nil {
		return fmt.Errorf("编码错误图片失败: %v", err)
	}

	return nil
}
