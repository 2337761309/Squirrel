package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"subdomain-checker/checker"
	"subdomain-checker/config"
	"subdomain-checker/screenshot"
	"subdomain-checker/utils"
	"subdomain-checker/view"
)

// 获取系统内存信息（GB）
func getSystemMemoryGB() float64 {
	if runtime.GOOS == "windows" {
		// Windows系统：使用wmic命令获取真实的系统内存
		cmd := exec.Command("wmic", "computersystem", "get", "TotalPhysicalMemory", "/value")
		output, err := cmd.Output()
		if err == nil {
			outputStr := string(output)
			// 解析输出，查找TotalPhysicalMemory=数值
			lines := strings.Split(outputStr, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "TotalPhysicalMemory=") {
					memoryStr := strings.TrimPrefix(line, "TotalPhysicalMemory=")
					memoryStr = strings.TrimSpace(memoryStr)
					if memoryBytes, err := strconv.ParseUint(memoryStr, 10, 64); err == nil {
						return float64(memoryBytes) / (1024 * 1024 * 1024) // 转换为GB
					}
				}
			}
		}

		// 如果wmic失败，尝试使用PowerShell
		cmd = exec.Command("powershell", "-Command", "(Get-CimInstance Win32_PhysicalMemory | Measure-Object -Property capacity -Sum).sum")
		output, err = cmd.Output()
		if err == nil {
			outputStr := strings.TrimSpace(string(output))
			if memoryBytes, err := strconv.ParseUint(outputStr, 10, 64); err == nil {
				return float64(memoryBytes) / (1024 * 1024 * 1024) // 转换为GB
			}
		}
	} else {
		// Linux/Mac系统：使用/proc/meminfo或其他方法
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if memoryKB, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
							return float64(memoryKB) / (1024 * 1024) // 转换为GB (KB -> GB)
						}
					}
				}
			}
		}
	}

	// 如果所有方法都失败，使用runtime估算（但给出警告）
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// 使用更保守的估算，假设至少16GB内存（现代计算机的常见配置）
	estimatedMemoryGB := 16.0
	fmt.Printf("⚠️  无法准确检测系统内存，估算为%.1fGB\n", estimatedMemoryGB)

	return estimatedMemoryGB
}

// 智能计算合理的截图并发数 - CPU+内存综合评估
func calculateOptimalScreenshotConcurrency(requestedConcurrency int, totalDomains int) int {
	// 获取系统资源信息
	numCPU := runtime.NumCPU()
	memoryGB := getSystemMemoryGB()

	// 显示系统资源信息
	fmt.Printf("💻 系统资源: CPU=%d核心, 内存=%.1fGB\n", numCPU, memoryGB)

	// 基于CPU计算推荐并发数 - 更激进的策略，充分利用多核
	var cpuBasedConcurrency int
	if numCPU >= 16 {
		cpuBasedConcurrency = numCPU * 2 // 高性能CPU：每核心2个Chrome实例
	} else if numCPU >= 8 {
		cpuBasedConcurrency = numCPU * 2 // 中等性能CPU：每核心2个
	} else if numCPU >= 4 {
		cpuBasedConcurrency = numCPU * 1 // 低性能CPU：每核心1个
	} else {
		cpuBasedConcurrency = numCPU // 极低性能CPU：总核心数
	}

	// 基于内存计算推荐并发数（每个Chrome实例约需150MB，更精确的估算）
	chromeMemoryPerInstance := 0.15            // 150MB per Chrome instance (优化后)
	availableMemoryForChrome := memoryGB * 0.7 // 使用70%的内存给Chrome (更激进)
	memoryBasedConcurrency := int(availableMemoryForChrome / chromeMemoryPerInstance)

	// 取CPU和内存限制的较小值
	optimalConcurrency := cpuBasedConcurrency
	limitingFactor := "CPU"
	if memoryBasedConcurrency < cpuBasedConcurrency {
		optimalConcurrency = memoryBasedConcurrency
		limitingFactor = "内存"
		fmt.Printf("🧠 内存成为限制因素: 内存支持最多%d个Chrome实例\n", memoryBasedConcurrency)
	} else {
		fmt.Printf("⚡ CPU成为限制因素: CPU支持最多%d个Chrome实例\n", cpuBasedConcurrency)
	}

	// 智能并发限制 - 基于系统稳定性和性能的动态调整
	// 针对大量域名（4万+）的特殊优化
	if totalDomains > 20000 {
		// 超大规模域名处理，强制降低并发
		if optimalConcurrency > 15 {
			optimalConcurrency = 15
			fmt.Printf("🔥 超大规模处理: 检测到%d个域名，限制为15个并发\n", totalDomains)
			fmt.Printf("💡 提示: 大量域名处理需要保守的并发数以避免系统崩溃\n")
		}
	} else if totalDomains > 10000 {
		// 大规模域名处理
		if optimalConcurrency > 25 {
			optimalConcurrency = 25
			fmt.Printf("🚀 大规模处理: 检测到%d个域名，限制为25个并发\n", totalDomains)
			fmt.Printf("💡 提示: 大量域名处理时，过高并发会导致网络错误增加\n")
		}
	} else if optimalConcurrency > 50 {
		optimalConcurrency = 50
		fmt.Printf("🚀 高并发限制: 限制为50个并发以避免网络拥塞\n")
		fmt.Printf("💡 提示: 处理大量域名时，过高并发会导致网络错误增加\n")
	}

	if optimalConcurrency > 30 {
		fmt.Printf("⚠️  中高并发模式: %d个并发，适合大量域名处理\n", optimalConcurrency)
		fmt.Printf("💡 建议: 监控网络错误率，如果过高请降低并发数\n")
	} else if optimalConcurrency > 20 {
		fmt.Printf("⚖️  平衡模式: %d个并发 (限制因素: %s)\n", optimalConcurrency, limitingFactor)
	} else {
		fmt.Printf("✅ 推荐并发数: %d个 (限制因素: %s)\n", optimalConcurrency, limitingFactor)
	}

	// 如果用户请求的并发数较小，使用用户设置
	if requestedConcurrency < optimalConcurrency {
		optimalConcurrency = requestedConcurrency
	}

	// 显示资源评估结果
	fmt.Printf("📈 资源评估: CPU支持%d个, 内存支持%d个, 推荐%d个\n",
		cpuBasedConcurrency, memoryBasedConcurrency, optimalConcurrency)

	// 根据最终并发数给出性能预期和建议
	if optimalConcurrency <= numCPU {
		fmt.Printf("✅ 稳定模式: %d个工作者 (预期成功率: 95%%+, 速度稳定)\n", optimalConcurrency)
		fmt.Printf("📈 性能预期: 低资源占用，高成功率，适合长时间运行\n")
	} else if optimalConcurrency <= numCPU*2 {
		fmt.Printf("⚖️  平衡模式: %d个工作者 (预期成功率: 85-95%%, 速度较快)\n", optimalConcurrency)
		fmt.Printf("📈 性能预期: 中等资源占用，良好成功率，速度与稳定性平衡\n")
	} else if optimalConcurrency <= numCPU*3 {
		fmt.Printf("⚡ 高速模式: %d个工作者 (预期成功率: 75-85%%, 高速度)\n", optimalConcurrency)
		fmt.Printf("📈 性能预期: 高资源占用，中等成功率，最大化处理速度\n")
	} else {
		fmt.Printf("🚀 极速模式: %d个工作者 (预期成功率: 60-75%%, 极高速度)\n", optimalConcurrency)
		fmt.Printf("📈 性能预期: 极高资源占用，可能出现更多失败，但处理速度最快\n")
		fmt.Printf("⚠️  警告: 建议监控系统资源使用情况\n")
	}

	// 如果用户请求的并发数过高，给出警告
	if requestedConcurrency > optimalConcurrency {
		fmt.Printf("🔧 智能优化: %d -> %d (基于CPU和内存资源自动调整)\n", requestedConcurrency, optimalConcurrency)
		fmt.Printf("💡 提示: 系统资源限制，使用推荐值可获得最佳性能\n")
	}

	// 确保至少有1个工作者
	if optimalConcurrency < 1 {
		optimalConcurrency = 1
	}

	// 确保不超过域名总数
	if optimalConcurrency > totalDomains {
		optimalConcurrency = totalDomains
	}

	return optimalConcurrency
}

// 清理所有Chrome进程
func cleanupChromeProcesses() {
	fmt.Printf("🧹 正在检查并清理Chrome进程...\n")

	cleanedCount := 0

	// Windows系统清理Chrome进程
	if runtime.GOOS == "windows" {
		// 首先检查是否有Chrome进程在运行
		checkCmd := exec.Command("tasklist", "/FI", "IMAGENAME eq chrome.exe")
		checkOutput, _ := checkCmd.CombinedOutput()

		if strings.Contains(string(checkOutput), "chrome.exe") {
			// 有Chrome进程在运行，需要清理
			chromeProcesses := []string{
				"chrome.exe",
				"chromedriver.exe",
			}

			for _, process := range chromeProcesses {
				cmd := exec.Command("taskkill", "/F", "/IM", process)
				output, err := cmd.CombinedOutput()
				if err == nil {
					fmt.Printf("✅ 已清理进程: %s\n", process)
					cleanedCount++
				} else {
					// 只在真正的错误时显示（不是"进程未找到"）
					outputStr := string(output)
					if !strings.Contains(outputStr, "没有找到进程") &&
						!strings.Contains(outputStr, "not found") &&
						!strings.Contains(outputStr, "No tasks") {
						fmt.Printf("⚠️  清理进程 %s 时出错: %v\n", process, err)
					}
				}
			}

			// 额外使用wmic命令清理残留的Chrome进程
			exec.Command("wmic", "process", "where", "name='chrome.exe'", "delete").Run()
		}

	} else {
		// Linux/Mac系统清理Chrome进程
		exec.Command("pkill", "-f", "chrome").Run()
		exec.Command("pkill", "-f", "chromium").Run()
		exec.Command("pkill", "-f", "google-chrome").Run()
		cleanedCount = 1 // 假设清理了一些进程
	}

	if cleanedCount > 0 {
		fmt.Printf("✅ Chrome进程清理完成，清理了 %d 个进程\n", cleanedCount)
	} else {
		fmt.Printf("✅ 无需清理，Chrome进程已正常退出\n")
	}
}

// 优雅关闭处理器
func setupGracefulShutdown(screenshotPool *screenshot.ScreenshotPool) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		fmt.Printf("\n🛑 接收到中断信号，正在优雅关闭...\n")

		// 停止截图工作池
		if screenshotPool != nil {
			fmt.Printf("📸 正在停止截图工作池...\n")
			screenshotPool.Stop()
		}

		// 清理Chrome进程
		cleanupChromeProcesses()

		fmt.Printf("👋 程序已安全退出\n")
		os.Exit(0)
	}()
}

func main() {
	// 确保程序退出时清理资源
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("🚨 程序异常退出: %v\n", r)
			cleanupChromeProcesses()
		}
	}()

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
                    松鼠子域名检测工具 v1.3
`)

	// 解析命令行参数
	cfg := config.Config{}
	config.ParseFlags(&cfg)

	// HTML输出选项
	var htmlOutput, simpleHTML string
	flag.StringVar(&htmlOutput, "html", "", "输出结果到HTML文件")
	flag.StringVar(&simpleHTML, "simple-html", "", "输出结果到简化版HTML文件")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("用法: squirrel [选项] <域名列表文件或逗号分隔的域名列表>")
		fmt.Println("\n选项:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if (cfg.Screenshot || cfg.ScreenshotAlive) && cfg.ExcelFile == "" && htmlOutput == "" && simpleHTML == "" {
		fmt.Println("错误: 启用截图功能时必须指定 -excel、-html 或 -simple-html 选项")
		os.Exit(1)
	}

	var domains []string
	var err error
	arg := flag.Arg(0)
	if strings.Contains(arg, ",") {
		domains = strings.Split(arg, ",")
	} else {
		domains, err = utils.ReadDomainsFromFile(arg)
		if err != nil {
			fmt.Printf("无法读取文件: %s\n", err)
			os.Exit(1)
		}
	}
	// 新增：归一化域名，支持 http(s):// 前缀
	domainMap := make(map[string]bool)
	var uniqueDomains []string
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			if u, err := url.Parse(d); err == nil && u.Host != "" {
				if !domainMap[u.Host] {
					domainMap[u.Host] = true
					uniqueDomains = append(uniqueDomains, u.Host)
				}
			} else {
				if !domainMap[d] {
					domainMap[d] = true
					uniqueDomains = append(uniqueDomains, d)
				}
			}
		} else {
			if !domainMap[d] {
				domainMap[d] = true
				uniqueDomains = append(uniqueDomains, d)
			}
		}
	}
	domains = uniqueDomains
	if len(domains) == 0 {
		fmt.Println("没有找到需要检测的域名")
		os.Exit(1)
	}

	fmt.Printf("总共需要检测 %d 个域名，并发数: %d，超时: %d秒\n",
		len(domains), cfg.Concurrency, cfg.Timeout)

	startTime := time.Now()
	totalDomains := len(domains)

	resultChan := make(chan checker.Result, totalDomains*2)
	domainChan := make(chan string, totalDomains)
	doneChan := make(chan struct{})
	progressDone := make(chan struct{})
	var wg sync.WaitGroup

	var screenshotPool *screenshot.ScreenshotPool
	if cfg.Screenshot || cfg.ScreenshotAlive {
		// 使用智能资源感知计算最优并发数
		screenshotWorkers := calculateOptimalScreenshotConcurrency(cfg.Concurrency, len(domains))

		// 设置全局并发数，用于动态调整超时
		screenshot.SetConcurrency(screenshotWorkers)

		fmt.Printf("🚀 最终截图并发数: %d 个工作者\n", screenshotWorkers)
		screenshotPool = screenshot.NewScreenshotPool(screenshotWorkers)
		screenshotPool.Start()

		// 设置优雅关闭处理器
		setupGracefulShutdown(screenshotPool)
	}

	var processed int32 = 0
	go view.ShowProgress(&processed, totalDomains, startTime, doneChan, progressDone)

	var resultsMutex sync.Mutex
	allResults := make([]checker.Result, 0, totalDomains)
	var alive, dead int32
	var pageTypeCountMutex sync.Mutex
	var pageTypeCount = make(map[string]int)
	var screenshotCount int32 = 0

	const batchSize = 10
	resultBatchChan := make(chan []checker.Result, totalDomains/batchSize+1)
	go func() {
		for resultBatch := range resultBatchChan {
			resultsMutex.Lock()
			for _, result := range resultBatch {
				if result.Alive {
					atomic.AddInt32(&alive, 1)
					if result.PageInfo != nil {
						pageTypeCountMutex.Lock()
						pageTypeCount[result.PageInfo.Type]++
						pageTypeCountMutex.Unlock()
					}
				} else {
					atomic.AddInt32(&dead, 1)
				}
				if result.Screenshot != "" {
					if cfg.ScreenshotAlive {
						if result.Alive {
							atomic.AddInt32(&screenshotCount, 1)
						}
					} else if cfg.Screenshot {
						atomic.AddInt32(&screenshotCount, 1)
					}
				}
				allResults = append(allResults, result)
			}
			resultsMutex.Unlock()
		}
	}()

	go func() {
		var resultBatch []checker.Result
		for result := range resultChan {
			atomic.AddInt32(&processed, 1)
			resultBatch = append(resultBatch, result)
			if len(resultBatch) >= batchSize || atomic.LoadInt32(&processed) == int32(totalDomains) {
				resultBatchChan <- resultBatch
				resultBatch = nil
			}
		}
		if len(resultBatch) > 0 {
			resultBatchChan <- resultBatch
		}
		close(resultBatchChan)
		close(doneChan)
	}()

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerId int) {
			defer wg.Done()
			for domain := range domainChan {
				checker.CheckDomain(domain, cfg, resultChan, screenshotPool)
			}
		}(i)
	}
	for _, domain := range domains {
		domainChan <- domain
	}
	close(domainChan)
	wg.Wait()

	// 在所有域名检查完成后，关闭截图工作池
	if screenshotPool != nil {
		fmt.Printf("📸 正在停止截图工作池...\n")
		screenshotPool.Stop()
	}

	close(resultChan)
	<-doneChan
	<-progressDone

	// 程序正常结束时清理资源
	if cfg.Screenshot || cfg.ScreenshotAlive {
		cleanupChromeProcesses()
	}

	fmt.Printf("\r%-80s\r", " ")
	totalTime := time.Since(startTime)
	view.PrintSummary(len(domains), int(atomic.LoadInt32(&alive)), int(atomic.LoadInt32(&dead)), &cfg, pageTypeCount, &pageTypeCountMutex, atomic.LoadInt32(&screenshotCount), totalTime)

	if cfg.OutputFile != "" {
		err := view.SaveResultsToFile(allResults, cfg.OutputFile)
		if err != nil {
			fmt.Printf("保存结果到文件时出错: %s\n", err)
		} else {
			fmt.Printf("结果已保存到 %s\n", cfg.OutputFile)
		}
	}
	if cfg.ExcelFile != "" {
		err := view.SaveResultsToExcel(allResults, cfg.ExcelFile, cfg.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到Excel文件时出错: %s\n", err)
		} else {
			fmt.Printf("结果已保存到 %s\n", cfg.ExcelFile)
		}
	}
	if htmlOutput != "" {
		err := view.SaveResultsToHTML(allResults, htmlOutput, cfg.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到HTML文件时出错: %s\n", err)
		} else {
			fmt.Printf("HTML报告已保存到 %s\n", htmlOutput)
		}
	}
	if simpleHTML != "" {
		err := view.SaveResultsToSimpleHTML(allResults, simpleHTML, cfg.OnlyAlive)
		if err != nil {
			fmt.Printf("保存结果到简化版HTML文件时出错: %s\n", err)
		} else {
			fmt.Printf("简化版HTML报告已保存到 %s\n", simpleHTML)
		}
	}
}
