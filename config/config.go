package config

import (
	"flag"
)

type Config struct {
	Timeout          int
	Concurrency      int
	Verbose          bool
	FollowRedirects  bool
	ShowResponseTime bool
	OutputFile       string
	ExcelFile        string
	ExtractInfo      bool
	OnlyAlive        bool
	Screenshot       bool
	ScreenshotAlive  bool
	ScreenshotDir    string
}

func ParseFlags(cfg *Config) {
	flag.IntVar(&cfg.Timeout, "timeout", 10, "请求超时时间(秒)")
	flag.IntVar(&cfg.Concurrency, "concurrency", 10, "并发数量")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "显示详细输出")
	flag.BoolVar(&cfg.FollowRedirects, "follow", false, "跟随重定向")
	flag.BoolVar(&cfg.ShowResponseTime, "time", false, "显示响应时间")
	flag.StringVar(&cfg.OutputFile, "output", "", "输出结果到CSV文件")
	flag.StringVar(&cfg.ExcelFile, "excel", "", "输出结果到Excel文件")
	flag.BoolVar(&cfg.ExtractInfo, "extract", false, "提取页面重要信息（登录页面等）")
	flag.BoolVar(&cfg.OnlyAlive, "only-alive", false, "只导出存活的域名")
	flag.BoolVar(&cfg.Screenshot, "screenshot", false, "对所有网页进行截图")
	flag.BoolVar(&cfg.ScreenshotAlive, "screenshot-alive", false, "只截图存活的网页")
	flag.StringVar(&cfg.ScreenshotDir, "screenshot-dir", "screenshots", "截图保存目录")
}
