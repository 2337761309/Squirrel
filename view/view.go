package view

import (
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"subdomain-checker/checker"
	"subdomain-checker/config"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 显示进度
func ShowProgress(processed *int32, totalDomains int, startTime time.Time, doneChan, progressDone chan struct{}) {
	// 启动进度显示goroutine
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(500 * time.Millisecond) // 更新频率提高到0.5秒一次
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt32(processed)
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
}

// 打印总结
func PrintSummary(total, alive, dead int, cfg *config.Config, pageTypeCount map[string]int, pageTypeCountMutex *sync.Mutex, screenshotCount int32, totalTime time.Duration) {
	// 打印表头
	fmt.Println("\n检测结果 (总结):")
	fmt.Println("----------------------------------------")

	// 输出总结
	fmt.Printf("总计: %d 个域名, %d 个存活, %d 个无法访问\n", total, alive, dead)

	// 如果启用了页面信息提取，显示页面类型统计
	if cfg.ExtractInfo && len(pageTypeCount) > 0 {
		fmt.Println("页面类型统计:")
		pageTypeCountMutex.Lock()
		for pageType, count := range pageTypeCount {
			fmt.Printf("  %s: %d 个\n", pageType, count)
		}
		pageTypeCountMutex.Unlock()
	}

	// 显示截图统计
	if cfg.Screenshot || cfg.ScreenshotAlive {
		if cfg.ScreenshotAlive {
			fmt.Printf("成功截图存活网站: %d 个\n", screenshotCount)
		} else {
			fmt.Printf("成功截图: %d 个\n", screenshotCount)
		}
	}

	fmt.Printf("检测耗时: %.2f 秒\n", totalTime.Seconds())
}

// 保存结果到文件
func SaveResultsToFile(results []checker.Result, filename string) error {
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
func SaveResultsToExcel(results []checker.Result, filename string, onlyAlive bool) error {
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

	// 创建模板数据
	data := TemplateData{
		TotalDomains: len(results),
		AliveDomains: 0,
		DeadDomains:  0,
		ReportTime:   time.Now().Format("2006-01-02 15:04:05"),
		Results:      make([]TemplateResult, 0, len(results)),
	}

	for _, result := range results {
		// 如果只导出存活的域名，则跳过非存活的
		if onlyAlive && !result.Alive {
			continue
		}

		// 更新统计数据
		if result.Alive {
			data.AliveDomains++
		} else {
			data.DeadDomains++
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

		// 处理域名链接
		domainLink := result.Domain
		if !strings.HasPrefix(domainLink, "http://") && !strings.HasPrefix(domainLink, "https://") {
			domainLink = "http://" + domainLink
		}

		// 处理截图路径
		screenshot := ""
		if result.Screenshot != "" {
			// 使用相对路径
			screenshot = filepath.Join("screenshots", filepath.Base(result.Screenshot))
			// 将路径分隔符转换为正斜杠，确保在HTML中正确显示
			screenshot = strings.ReplaceAll(screenshot, "\\", "/")
			// 确保路径以screenshots/开头
			if !strings.HasPrefix(screenshot, "screenshots/") {
				screenshot = "screenshots/" + filepath.Base(screenshot)
			}
		}

		// 处理标题编码
		title := result.Title
		if title != "" {
			// 尝试检测并转换编码
			if !utf8.ValidString(title) {
				// 尝试从GBK转换到UTF-8
				reader := transform.NewReader(strings.NewReader(title), simplifiedchinese.GBK.NewDecoder())
				if d, err := io.ReadAll(reader); err == nil {
					title = string(d)
				}
			}
		}

		// 设置状态样式
		statusClass := "status-dead"
		domainStatus := "无法访问"
		if result.Alive {
			statusClass = "status-alive"
			domainStatus = "可访问"
		}

		// 添加到结果列表
		data.Results = append(data.Results, TemplateResult{
			Domain:       result.Domain,
			DomainLink:   domainLink,
			StatusClass:  statusClass,
			DomainStatus: domainStatus,
			StatusText:   result.StatusText,
			Status:       result.Status,
			ResponseTime: result.ResponseTime.Seconds() * 1000,
			PageType:     pageType,
			Title:        title,
			Message:      result.Message,
			Screenshot:   screenshot,
			Alive:        result.Alive,
		})

		// 在主表中添加"查看截图"超链接
		if result.Screenshot != "" {
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
				},
			})
			f.SetCellStyle(sheetName, fmt.Sprintf("H%d", row), fmt.Sprintf("H%d", row), linkStyle)
			f.SetCellHyperLink(sheetName, fmt.Sprintf("H%d", row), screenshot, "External")
		} else {
			f.SetCellValue(sheetName, fmt.Sprintf("H%d", row), "无截图")
			f.SetCellStyle(sheetName, fmt.Sprintf("H%d", row), fmt.Sprintf("H%d", row), contentStyle)
		}

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

// 定义模板数据结构
type TemplateData struct {
	TotalDomains int
	AliveDomains int
	DeadDomains  int
	ReportTime   string
	Results      []TemplateResult
}

// 定义单个域名结果的数据结构
type TemplateResult struct {
	Domain       string
	DomainLink   string
	StatusClass  string
	DomainStatus string
	StatusText   string
	Status       int
	ResponseTime float64
	PageType     string
	Title        string
	Message      string
	Screenshot   string
	Alive        bool
}

// 保存结果到HTML文件（简化版）
func SaveResultsToSimpleHTML(results []checker.Result, filename string, onlyAlive bool) error {
	// 创建HTML文件
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// 写入UTF-8 BOM
	file.Write([]byte{0xEF, 0xBB, 0xBF})

	// 计算统计信息并准备模板数据
	data := TemplateData{
		ReportTime: time.Now().Format("2006-01-02 15:04:05"),
	}

	// 处理结果数据
	for _, result := range results {
		// 如果只显示存活域名，跳过非存活的
		if onlyAlive && !result.Alive {
			continue
		}

		data.TotalDomains++
		if result.Alive {
			data.AliveDomains++
		}

		// 准备单个结果数据
		statusClass := "status-dead"
		domainStatus := "dead"
		if result.Alive {
			statusClass = "status-alive"
			domainStatus = "alive"
		}

		pageType := "-"
		if result.PageInfo != nil {
			pageType = result.PageInfo.Type
		}

		// 处理域名链接
		domainLink := result.Domain
		if !strings.HasPrefix(domainLink, "http://") && !strings.HasPrefix(domainLink, "https://") {
			domainLink = "http://" + domainLink
		}

		// 处理截图路径
		screenshot := ""
		if result.Screenshot != "" {
			// 使用相对路径
			screenshot = filepath.Join("screenshots", filepath.Base(result.Screenshot))
			// 将路径分隔符转换为正斜杠，确保在HTML中正确显示
			screenshot = strings.ReplaceAll(screenshot, "\\", "/")
			// 确保路径以screenshots/开头
			if !strings.HasPrefix(screenshot, "screenshots/") {
				screenshot = "screenshots/" + filepath.Base(screenshot)
			}
		}

		// 处理标题编码
		title := result.Title
		if title != "" {
			// 尝试检测并转换编码
			if !utf8.ValidString(title) {
				// 尝试从GBK转换到UTF-8
				reader := transform.NewReader(strings.NewReader(title), simplifiedchinese.GBK.NewDecoder())
				if d, err := io.ReadAll(reader); err == nil {
					title = string(d)
				}
			}
		}

		data.Results = append(data.Results, TemplateResult{
			Domain:       result.Domain,
			DomainLink:   domainLink,
			StatusClass:  statusClass,
			DomainStatus: domainStatus,
			StatusText:   result.StatusText,
			Status:       result.Status,
			ResponseTime: result.ResponseTime.Seconds() * 1000,
			PageType:     pageType,
			Title:        title,
			Message:      result.Message,
			Screenshot:   screenshot,
			Alive:        result.Alive,
		})
	}
	data.DeadDomains = data.TotalDomains - data.AliveDomains

	// 解析模板文件
	tmpl, err := template.ParseFiles("view/template.html")
	if err != nil {
		return fmt.Errorf("解析模板文件失败: %v", err)
	}

	// 执行模板并写入结果
	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("执行模板失败: %v", err)
	}

	return nil
}

// 保存结果到HTML文件（带详细信息）
func SaveResultsToHTML(results []checker.Result, filename string, onlyAlive bool) error {
	return SaveResultsToSimpleHTML(results, filename, onlyAlive)
}
