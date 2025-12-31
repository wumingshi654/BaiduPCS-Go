package pcscommand

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
	"github.com/qjfoidnh/BaiduPCS-Go/pcsutil"
)

// syncFileState 记录文件的修改时间、大小和MD5
type syncFileState struct {
	ModTime int64  `json:"mod_time"`
	Size    int64  `json:"size"`
	MD5     string `json:"md5,omitempty"`
}

// syncConfig 同步配置信息（保存加密设置等）
type syncConfig struct {
	EncryptKey    string                 `json:"encrypt_key,omitempty"`
	EncryptMethod string                 `json:"encrypt_method,omitempty"`
	Files         map[string]syncFileState `json:"files"`
}

// loadState 从状态文件读取配置和文件状态
func loadState(stateFile string) (*syncConfig, error) {
	cfg := &syncConfig{
		Files: make(map[string]syncFileState),
	}
	b, err := ioutil.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// saveState 将配置和文件状态保存到文件
func saveState(stateFile string, cfg *syncConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(stateFile, b, 0644)
}

// RunSync 在 localDir 下每隔 intervalSeconds 检测变更并上传到 remoteDir
// 如果提供了 encryptKey，则在上传前加密文件
func RunSync(localDir, remoteDir string, intervalSeconds int, stateFile, encryptKey, encryptMethod string) error {
	// 规范化路径
	localDir = filepath.Clean(localDir)
	if stateFile == "" {
		stateFile = filepath.Join(localDir, ".pcs_sync_state.json")
	}

	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()

	// 捕获中断信号
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	// 读取初始配置和状态
	cfg, err := loadState(stateFile)
	if err != nil {
		fmt.Printf("加载同步状态失败: %s\n", err)
		cfg = &syncConfig{
			Files: make(map[string]syncFileState),
		}
	}

	// 保存加密设置到配置中（仅在首次或密钥改变时）
	if encryptKey != "" {
		cfg.EncryptKey = encryptKey
		cfg.EncryptMethod = encryptMethod
	}

	fmt.Printf("开始执行任务 本地目录 %s 同步 -> %s, 间隔 %d 秒, 状态文件: %s\n", localDir, remoteDir, intervalSeconds, stateFile)
	if encryptKey != "" {
		fmt.Printf("启用加密: 方法=%s\n", encryptMethod)
	}

	// 执行同步逻辑
	doSync := func() {
		walkedFiles, err := pcsutil.WalkDir(localDir, "")
		if err != nil {
			fmt.Printf("遍历目录错误: %s\n", err)
			return
		}

		for _, f := range walkedFiles {
			sysPath := f
			info, err := os.Stat(sysPath)
			if err != nil {
				continue
			}
			if info.IsDir() {
				continue
			}

			// 计算相对于 localDir 的相对路径（unix 风格便于跨平台）
			fileUnix := filepath.ToSlash(filepath.Clean(sysPath))
			baseUnix := filepath.ToSlash(localDir)
			rel := strings.TrimPrefix(fileUnix, baseUnix)
			rel = strings.TrimPrefix(rel, "/")

			modTime := info.ModTime().Unix()
			size := info.Size()

			// 计算文件的MD5
			currentMD5, err := md5sum(sysPath)
			if err != nil {
				fmt.Printf("计算文件 %s 的MD5失败: %s, 跳过\n", rel, err)
				continue
			}

			// 检查文件是否有变化
			prev, ok := cfg.Files[rel]
			if ok {
				if prev.MD5 == currentMD5 {
					// 文件内容未变化，跳过
					continue
				}
				fmt.Printf("文件 %s 的MD5与配置中不一致: prev_md5=%s new_md5=%s, 执行上传\n", rel, prev.MD5, currentMD5)
			} else {
				fmt.Printf("文件 %s 未在配置中, 执行首次上传\n", rel)
			}

			// 确定上传的文件
			uploadPath := sysPath
			defer func(path string) {
				// 上传完成后清理临时加密文件
				if path != sysPath && fileExists(path) {
					os.Remove(path)
				}
			}(uploadPath)

			// 如果启用加密，先加密文件到临时位置
			if encryptKey != "" {
				tempEncrypted := sysPath + ".encrypted"
				if err := encryptFileForSync(sysPath, tempEncrypted, encryptKey, encryptMethod); err != nil {
					fmt.Printf("加密失败: %s, 跳过上传\n", err)
					continue
				}
				uploadPath = tempEncrypted
				fmt.Printf("文件已加密: %s\n", tempEncrypted)
			}

			// 计算上传目标路径
			relDir := filepath.Dir(rel)
			if relDir == "." {
				relDir = ""
			}
			var savePath string
			if relDir == "" {
				savePath = remoteDir
			} else {
				savePath = path.Clean(remoteDir + baidupcs.PathSeparator + filepath.ToSlash(relDir))
			}

			fmt.Printf("上传到: %s\n", savePath)

			// 调用上传（仅上传单个文件）
			RunUpload([]string{uploadPath}, savePath, &UploadOptions{})

			// 更新状态：记录原始文件的 mtime、size 和 MD5
			cfg.Files[rel] = syncFileState{
				ModTime: modTime,
				Size:    size,
				MD5:     currentMD5,
			}

			// 及时保存状态
			if err := saveState(stateFile, cfg); err != nil {
				fmt.Printf("保存状态失败: %s\n", err)
			}
		}
		// 本次扫描完成
		next := time.Now().Add(time.Duration(intervalSeconds) * time.Second).Format(time.RFC3339)
		fmt.Printf("%s 同步完成, 下次同步时间为 %s\n", localDir, next)
	}

	// 首次执行
	doSync()

	// 定期检测
	for {
		select {
		case <-ticker.C:
			doSync()
		case <-sigs:
			fmt.Printf("\n收到中断信号，保存状态并退出...\n")
			if err := saveState(stateFile, cfg); err != nil {
				fmt.Printf("保存状态失败: %s\n", err)
			}
			return nil
		}
	}
}

// fileExists 检查文件是否存在
func fileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

// encryptFileForSync 加密文件用于同步，保存到新文件并返回
func encryptFileForSync(inputPath, outputPath, key, method string) error {
	// 使用 pcsutil 中的加密函数
	encryptedPath, err := pcsutil.EncryptFile(method, []byte(key), inputPath, true)
	if err != nil {
		return err
	}

	// EncryptFile 返回的是源文件被改名后的加密文件路径
	// 我们需要将其移到目标位置
	if err := os.Rename(encryptedPath, outputPath); err != nil {
		return err
	}

	return nil
}

// md5sum 计算文件的MD5哈希值
func md5sum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
