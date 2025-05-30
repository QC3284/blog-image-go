package main

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 配置结构体
type Config struct {
	BaseURL      string
	SaveDir      string
	Verbose      bool
	InfiniteRetry bool
	Concurrency  int
}

var (
	downloaded      = make(map[string]bool)
	nextNum         = 1
	mutex           sync.Mutex
	downloadCounter int
	wg              sync.WaitGroup
)

func main() {
	// 打印免责声明
	printDisclaimer()

	// 获取用户配置
	cfg := getUserConfig()

	// 准备下载目录
	if err := prepareSaveDir(cfg.SaveDir); err != nil {
		log.Fatalf("❌ 无法创建保存目录: %v", err)
	}

	// 扫描现有文件
	if err := scanExistingFiles(cfg.SaveDir); err != nil {
		log.Fatalf("❌ 扫描现有文件失败: %v", err)
	}

	log.Printf("🚀 开始下载任务 (线程数: %d, 保存目录: '%s')", cfg.Concurrency, cfg.SaveDir)

	// 创建任务通道
	taskChan := make(chan struct{}, cfg.Concurrency*2)

	// 启动工作协程
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go worker(i+1, cfg, taskChan)
	}

	// 添加初始任务
	for i := 0; i < cfg.Concurrency*2; i++ {
		taskChan <- struct{}{}
	}

	// 定期添加新任务
	go func() {
		for range time.Tick(500 * time.Millisecond) {
			taskChan <- struct{}{}
		}
	}()

	// 等待所有工作协程完成
	wg.Wait()
	log.Println("✅ 所有下载任务已完成")
}

func printDisclaimer() {
	fmt.Println("============================================================")
	fmt.Println("                     免责声明")
	fmt.Println("============================================================")
	fmt.Println("1. 本工具仅用于技术学习和研究目的")
	fmt.Println("2. 请确保您有权下载和使用目标图片")
	fmt.Println("3. 开发者不对使用本工具导致的任何后果负责")
	fmt.Println("4. 请遵守目标网站的服务条款和相关法律法规")
	fmt.Println("============================================================")
	fmt.Println()
}

func getUserConfig() Config {
	cfg := Config{
		BaseURL: "https://kasuie.cc/api/img/bg?size=regular",
		SaveDir: ".",
	}

	// 获取 API 地址
	fmt.Print("请输入API地址(直接回车使用默认): ")
	var inputURL string
	fmt.Scanln(&inputURL)
	if strings.TrimSpace(inputURL) != "" {
		cfg.BaseURL = inputURL
	}

	// 获取保存目录
	fmt.Print("请输入保存目录(直接回车使用当前目录): ")
	var saveDir string
	fmt.Scanln(&saveDir)
	if strings.TrimSpace(saveDir) != "" {
		cfg.SaveDir = saveDir
	}

	// 是否开启详细日志
	fmt.Print("是否开启详细日志? (y/n): ")
	var verboseInput string
	fmt.Scanln(&verboseInput)
	cfg.Verbose = strings.ToLower(verboseInput) == "y"

	// 是否无限重试
	fmt.Print("是否忽略错误无限重试? (y/n): ")
	var retryInput string
	fmt.Scanln(&retryInput)
	cfg.InfiniteRetry = strings.ToLower(retryInput) == "y"

	// 获取并发数
	fmt.Print("请输入并发数(线程数量): ")
	var concurrencyInput string
	fmt.Scanln(&concurrencyInput)
	concurrency, err := strconv.Atoi(concurrencyInput)
	if err != nil || concurrency < 1 {
		concurrency = 1
	}
	cfg.Concurrency = concurrency

	return cfg
}

func prepareSaveDir(dir string) error {
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}

func scanExistingFiles(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		matched, _ := regexp.MatchString(`^\d+\.jpg$`, file.Name())
		if matched {
			fullPath := filepath.Join(dir, file.Name())
			data, err := os.ReadFile(fullPath)
			if err != nil {
				continue
			}

			hash := md5.Sum(data)
			hashStr := hex.EncodeToString(hash[:])
			downloaded[hashStr] = true

			if num, err := strconv.Atoi(file.Name()[:len(file.Name())-4]); err == nil {
				if num >= nextNum {
					nextNum = num + 1
				}
			}
		}
	}
	return nil
}

func worker(id int, cfg Config, taskChan <-chan struct{}) {
	defer wg.Done()
	
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for range taskChan {
		if !cfg.InfiniteRetry && downloadCounter >= cfg.Concurrency {
			return
		}

		if cfg.Verbose {
			log.Printf("[Worker %d] 🔍 获取重定向地址: %s", id, cfg.BaseURL)
		}

		// 获取重定向URL
		resp, err := client.Head(cfg.BaseURL)
		if err != nil {
			handleError(id, cfg, fmt.Errorf("请求失败: %v", err))
			continue
		}

		if resp.StatusCode != http.StatusFound {
			handleError(id, cfg, fmt.Errorf("无效状态码: %d", resp.StatusCode))
			resp.Body.Close()
			continue
		}

		imgURL := resp.Header.Get("Location")
		if imgURL == "" {
			handleError(id, cfg, errors.New("缺少重定向地址"))
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		if cfg.Verbose {
			log.Printf("[Worker %d] 🔄 重定向至: %s", id, imgURL)
		}

		// 下载图片
		imgResp, err := client.Get(imgURL)
		if err != nil {
			handleError(id, cfg, fmt.Errorf("下载失败: %v", err))
			continue
		}

		imgData, err := io.ReadAll(imgResp.Body)
		imgResp.Body.Close()
		if err != nil {
			handleError(id, cfg, fmt.Errorf("读取失败: %v", err))
			continue
		}

		// 检查唯一性
		hash := md5.Sum(imgData)
		hashStr := hex.EncodeToString(hash[:])

		mutex.Lock()
		if downloaded[hashStr] {
			mutex.Unlock()
			if cfg.Verbose {
				log.Printf("[Worker %d] ⚠️ 检测到重复图片 (MD5: %s)", id, hashStr)
			}
			continue
		}

		// 分配文件名
		filename := fmt.Sprintf("%d.jpg", nextNum)
		nextNum++
		downloadCounter++
		downloaded[hashStr] = true
		mutex.Unlock()

		// 保存文件
		fullPath := filepath.Join(cfg.SaveDir, filename)
		if err := os.WriteFile(fullPath, imgData, 0644); err != nil {
			handleError(id, cfg, fmt.Errorf("保存失败: %v", err))
			continue
		}

		log.Printf("[Worker %d] ✅ 已保存: %s (MD5: %s)", id, fullPath, hashStr)
		
		if !cfg.InfiniteRetry && downloadCounter >= cfg.Concurrency {
			return
		}
	}
}

func handleError(workerID int, cfg Config, err error) {
	log.Printf("[Worker %d] ❌ 错误: %v", workerID, err)
	if !cfg.InfiniteRetry {
		downloadCounter++
	}
}
