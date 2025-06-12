package screenshot

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
				if err := TakeScreenshotWithContext(ctx, task.URL, screenshotPath); err == nil {
					task.Result <- screenshotPath
				} else {
					// 截图失败，生成错误图片
					errorFilename := "error_" + task.Filename
					errorPath := filepath.Join(task.Dir, errorFilename)
					GenerateErrorImage(errorFilename, task.Dir)
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
func TakeScreenshotWithContext(ctx context.Context, url string, screenshotPath string) error {
	// 准备截图缓冲区
	var buf []byte

	// 检查URL是否包含协议前缀
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}

	// 设置超时
	taskCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 执行截图
	tasks := chromedp.Tasks{
		chromedp.Navigate(url),
		// 等待页面加载
		chromedp.Sleep(3 * time.Second),
		// 等待页面稳定
		chromedp.WaitReady("body", chromedp.ByQuery),
		// 获取页面滚动高度
		chromedp.Evaluate(`Math.max(document.body.scrollHeight, document.documentElement.scrollHeight)`, nil),
		// 使用FullScreenshot
		chromedp.FullScreenshot(&buf, 80), // 降低质量以提高性能
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
