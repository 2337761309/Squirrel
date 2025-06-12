package view

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"subdomain-checker/checker"
	"subdomain-checker/config"

	"github.com/xuri/excelize/v2"
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

// 保存结果到HTML文件（简化版）
func SaveResultsToSimpleHTML(results []checker.Result, filename string, onlyAlive bool) error {
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
func SaveResultsToHTML(results []checker.Result, filename string, onlyAlive bool) error {
	return SaveResultsToSimpleHTML(results, filename, onlyAlive)
}
