package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"subdomain-checker/checker"
	"subdomain-checker/config"
	"subdomain-checker/screenshot"
	"subdomain-checker/utils"
	"subdomain-checker/view"
)

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
		// 根据CPU核心数和域名数量动态调整截图工作池大小
		screenshotWorkers := runtime.NumCPU()
		if screenshotWorkers > len(domains) {
			screenshotWorkers = len(domains)
		}
		if screenshotWorkers < 1 {
			screenshotWorkers = 1
		}
		screenshotPool = screenshot.NewScreenshotPool(screenshotWorkers)
		screenshotPool.Start()
		defer screenshotPool.Stop()
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
	close(resultChan)
	<-doneChan
	<-progressDone

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
