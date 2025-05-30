package main

import (
	"crypto/md5"
	"encoding/hex"
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

// 免责声明常量
const disclaimer = `
免责声明：
----------------------------------------
本工具仅用于技术学习目的，下载的图片版权归原作者所有。
使用者应对下载内容负责，禁止用于任何非法用途。
开发者不对使用者行为承担任何法律责任。
----------------------------------------
`

type Downloader struct {
	client           *http.Client
	saveDir          string
	apiURL           string
	verbose          bool
	infiniteRetry    bool
	maxWorkers       int
	wg               sync.WaitGroup
	mu               sync.Mutex
	counter          int
	downloadedHashes map[string]bool
	workerTasks      map[int]chan struct{}
}

func main() {
	fmt.Println(disclaimer)

	// 获取用户输入
	var apiURL, saveDir, verboseInput, retryInput string
	fmt.Print("请输入API地址(直接回车使用默认): ")
	fmt.Scanln(&apiURL)
	if apiURL == "" {
		apiURL = "https://kasuie.cc/api/img/bg?size=regular"
	}

	fmt.Print("请输入保存目录(直接回车使用当前目录): ")
	fmt.Scanln(&saveDir)
	if saveDir == "" {
		saveDir = "."
	}

	fmt.Print("是否开启详细日志? (y/n): ")
	fmt.Scanln(&verboseInput)
	verbose := strings.ToLower(verboseInput) == "y"

	fmt.Print("是否忽略错误无限重试? (y/n): ")
	fmt.Scanln(&retryInput)
	infiniteRetry := strings.ToLower(retryInput) == "y"

	fmt.Print("请输入并发线程数(1-128): ")
	var maxWorkers int
	_, err := fmt.Scanln(&maxWorkers)
	if err != nil || maxWorkers < 1 {
		maxWorkers = 1
	} else if maxWorkers > 128 {
		maxWorkers = 128
	}

	// 创建下载器实例
	downloader := &Downloader{
		apiURL:           apiURL,
		saveDir:          saveDir,
		verbose:          verbose,
		infiniteRetry:    infiniteRetry,
		maxWorkers:       maxWorkers,
		downloadedHashes: make(map[string]bool),
		workerTasks:      make(map[int]chan struct{}),
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 不自动重定向
			},
		},
	}

	// 创建保存目录
	if err := os.MkdirAll(saveDir, 0755); err != nil {
		log.Fatalf("创建目录失败: %v", err)
	}

	// 扫描现有文件
	if err := downloader.scanExistingFiles(); err != nil {
		log.Fatalf("扫描文件失败: %v", err)
	}

	fmt.Printf("开始下载，线程数: %d, 保存目录: %s\n", maxWorkers, saveDir)

	// 启动工作协程
	for i := 0; i < maxWorkers; i++ {
		downloader.wg.Add(1)
		taskChan := make(chan struct{}, 1)
		downloader.workerTasks[i] = taskChan
		go downloader.worker(i, taskChan)
	}

	// 分配任务
	go func() {
		taskCount := 0
		for {
			for workerID, ch := range downloader.workerTasks {
				select {
				case ch <- struct{}{}:
					taskCount++
					if downloader.verbose {
						fmt.Printf("[分配器] 已向工作线程 %d 分配任务 #%d\n", workerID, taskCount)
					}
				default:
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// 等待所有工作协程结束
	downloader.wg.Wait()
	fmt.Println("所有任务已完成")
}

func (d *Downloader) scanExistingFiles() error {
	files, err := os.ReadDir(d.saveDir)
	if err != nil {
		return err
	}

	pattern := regexp.MustCompile(`^(\d+)\.jpg$`)
	maxNum := 0

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		matches := pattern.FindStringSubmatch(file.Name())
		if matches != nil {
			num, err := strconv.Atoi(matches[1])
			if err != nil {
				continue
			}

			if num > maxNum {
				maxNum = num
			}

			filePath := filepath.Join(d.saveDir, file.Name())
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}

			hash := md5.Sum(data)
			hashStr := hex.EncodeToString(hash[:])
			d.downloadedHashes[hashStr] = true
		}
	}

	d.counter = maxNum + 1
	fmt.Printf("扫描到 %d 个现有文件，下一个序号: %d\n", len(d.downloadedHashes), d.counter)
	return nil
}

func (d *Downloader) worker(id int, taskChan <-chan struct{}) {
	defer d.wg.Done()

	for range taskChan {
		startTime := time.Now()
		success := false
		defer func() {
			if d.verbose {
				duration := time.Since(startTime)
				status := "成功"
				if !success {
					status = "失败"
				}
				fmt.Printf("[Worker %d] 任务完成: %s, 耗时: %v\n", id, status, duration)
			}
		}()

		if d.verbose {
			fmt.Printf("[Worker %d] 开始新任务\n", id)
		}

		var imgURL string
		var imgData []byte
		var err error

		// 尝试302模式
		if d.verbose {
			fmt.Printf("[Worker %d] 尝试302重定向模式\n", id)
		}

		imgURL, imgData, err = d.downloadWithRedirect()
		if err != nil {
			if d.verbose {
				fmt.Printf("[Worker %d] 302模式失败: %v\n", id, err)
				fmt.Printf("[Worker %d] 尝试直接下载模式\n", id)
			}

			// 尝试直接下载模式
			imgURL, imgData, err = d.downloadDirect()
			if err != nil {
				if d.verbose {
					fmt.Printf("[Worker %d] 直接下载失败: %v\n", id, err)
				}
				if !d.infiniteRetry {
					return
				}
				continue
			}
		}

		if d.verbose {
			fmt.Printf("[Worker %d] 下载成功, URL: %s, 大小: %d bytes\n", id, imgURL, len(imgData))
		}

		// 检查唯一性
		hash := md5.Sum(imgData)
		hashStr := hex.EncodeToString(hash[:])

		d.mu.Lock()
		if d.downloadedHashes[hashStr] {
			d.mu.Unlock()
			if d.verbose {
				fmt.Printf("[Worker %d] 图片重复, 跳过\n", id)
			}
			if !d.infiniteRetry {
				return
			}
			continue
		}

		// 分配序号
		filename := fmt.Sprintf("%d.jpg", d.counter)
		d.counter++
		d.downloadedHashes[hashStr] = true
		d.mu.Unlock()

		// 保存文件
		filePath := filepath.Join(d.saveDir, filename)
		if err := os.WriteFile(filePath, imgData, 0644); err != nil {
			fmt.Printf("[Worker %d] 保存文件失败: %v\n", id, err)
			if !d.infiniteRetry {
				return
			}
			continue
		}

		fmt.Printf("[Worker %d] 图片已保存: %s\n", id, filePath)
		success = true
	}
}

func (d *Downloader) downloadWithRedirect() (string, []byte, error) {
	// 发送HEAD请求获取重定向URL
	req, err := http.NewRequest("HEAD", d.apiURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("创建请求失败: %w", err)
	}
	
	if d.verbose {
		fmt.Println("请求头:")
		for k, v := range req.Header {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	
	resp, err := d.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("HEAD请求失败: %w", err)
	}
	defer resp.Body.Close()

	if d.verbose {
		fmt.Printf("HEAD响应状态: %d\n", resp.StatusCode)
		fmt.Println("响应头:")
		for k, v := range resp.Header {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}

	if resp.StatusCode != http.StatusFound {
		return "", nil, fmt.Errorf("非302状态码: %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return "", nil, fmt.Errorf("缺少Location头")
	}

	if d.verbose {
		fmt.Printf("重定向地址: %s\n", location)
	}

	// 下载图片
	imgResp, err := d.client.Get(location)
	if err != nil {
		return location, nil, fmt.Errorf("图片下载失败: %w", err)
	}
	defer imgResp.Body.Close()

	if d.verbose {
		fmt.Printf("图片响应状态: %d\n", imgResp.StatusCode)
	}

	if imgResp.StatusCode != http.StatusOK {
		return location, nil, fmt.Errorf("图片服务器返回错误: %d", imgResp.StatusCode)
	}

	imgData, err := io.ReadAll(imgResp.Body)
	if err != nil {
		return location, nil, fmt.Errorf("读取图片失败: %w", err)
	}

	// 验证图片有效性
	if len(imgData) == 0 {
		return location, nil, fmt.Errorf("图片数据为空")
	}

	return location, imgData, nil
}

func (d *Downloader) downloadDirect() (string, []byte, error) {
	// 直接下载API返回的内容
	req, err := http.NewRequest("GET", d.apiURL, nil)
	if err != nil {
		return d.apiURL, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	
	if d.verbose {
		fmt.Println("请求头:")
		for k, v := range req.Header {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	
	resp, err := d.client.Do(req)
	if err != nil {
		return d.apiURL, nil, fmt.Errorf("GET请求失败: %w", err)
	}
	defer resp.Body.Close()

	if d.verbose {
		fmt.Printf("直接下载响应状态: %d\n", resp.StatusCode)
		fmt.Println("响应头:")
		for k, v := range resp.Header {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}

	if resp.StatusCode != http.StatusOK {
		return d.apiURL, nil, fmt.Errorf("非200状态码: %d", resp.StatusCode)
	}

	// 检查内容类型
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		// 可能是重定向到HTML页面
		if d.verbose {
			fmt.Println("响应内容不是图片，尝试解析可能的URL")
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return d.apiURL, nil, fmt.Errorf("读取响应体失败: %w", err)
		}

		// 尝试从HTML中提取图片URL
		if imgURL := extractImageURL(body); imgURL != "" {
			if d.verbose {
				fmt.Printf("从HTML中提取到图片URL: %s\n", imgURL)
			}
			return d.downloadDirectFromURL(imgURL)
		}

		return d.apiURL, nil, fmt.Errorf("内容类型不是图片: %s", contentType)
	}

	imgData, err := io.ReadAll(resp.Body)
	if err != nil {
		return d.apiURL, nil, fmt.Errorf("读取图片失败: %w", err)
	}

	if len(imgData) == 0 {
		return d.apiURL, nil, fmt.Errorf("图片数据为空")
	}

	return d.apiURL, imgData, nil
}

func (d *Downloader) downloadDirectFromURL(imgURL string) (string, []byte, error) {
	req, err := http.NewRequest("GET", imgURL, nil)
	if err != nil {
		return imgURL, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	
	if d.verbose {
		fmt.Println("图片请求头:")
		for k, v := range req.Header {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	
	resp, err := d.client.Do(req)
	if err != nil {
		return imgURL, nil, fmt.Errorf("图片下载失败: %w", err)
	}
	defer resp.Body.Close()

	if d.verbose {
		fmt.Printf("图片响应状态: %d\n", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return imgURL, nil, fmt.Errorf("图片服务器返回错误: %d", resp.StatusCode)
	}

	imgData, err := io.ReadAll(resp.Body)
	if err != nil {
		return imgURL, nil, fmt.Errorf("读取图片失败: %w", err)
	}

	if len(imgData) == 0 {
		return imgURL, nil, fmt.Errorf("图片数据为空")
	}

	return imgURL, imgData, nil
}

func extractImageURL(html []byte) string {
	// 简单查找图片URL的模式
	patterns := []string{
		`<img[^>]+src="([^"]+)"`,
		`url\(['"]?([^'")]+)['"]?\)`,
		`href="([^"]+\.(jpg|jpeg|png|gif|bmp|webp))"`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindSubmatch(html)
		if len(matches) > 1 {
			return string(matches[1])
		}
	}
	return ""
}
