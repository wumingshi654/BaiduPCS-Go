package pcscommand

import (
    "archive/zip"
    "crypto/sha1"
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
    "sync"
    "syscall"
    "time"

    "github.com/google/uuid"
    "github.com/qjfoidnh/BaiduPCS-Go/baidupcs"
    "github.com/qjfoidnh/BaiduPCS-Go/internal/pcsconfig"
    "github.com/qjfoidnh/BaiduPCS-Go/pcsutil"
)

const syncConfigFileName = "sync_config.json"

type WatchEntry struct {
    Local   string                       `json:"local"`
    Remote  string                       `json:"remote"`
    Interval int                         `json:"interval"`
    Key     string                       `json:"key,omitempty"`
    Method  string                       `json:"method,omitempty"`
    IgnoreFile string                     `json:"ignore_file,omitempty"`
    RandomFilename bool                  `json:"random_filename,omitempty"`
    // Merge 模式：合并上传（每隔 interval 打包目录并上传），而不是监控变化
    Merge bool                           `json:"merge,omitempty"`
    // MergeFilename 当启用随机文件名时, 合并上传的固定随机文件名
    MergeFilename string                 `json:"merge_filename,omitempty"`
    Files   map[string]syncFileState     `json:"files"`
    FileNameMap map[string]string        `json:"file_name_map,omitempty"`

    // runtime
    stopCh  chan struct{}                `json:"-"`
    // Running is runtime-only and should not be persisted to disk
    Running bool                         `json:"-"`
    patterns []ignorePattern            `json:"-"`
}

type SyncConfigFile struct {
    Watches map[string]*WatchEntry `json:"watches"`
}

var (
    mgrMu sync.Mutex
    mgr   = &syncManager{}
)

type syncManager struct{
    cfgPath string
    cfg     *SyncConfigFile
    mu      sync.Mutex
}

func (s *syncManager) configPath() string {
    if s.cfgPath != "" {
        return s.cfgPath
    }
    return filepath.Join(pcsconfig.GetConfigDir(), syncConfigFileName)
}

func (s *syncManager) load() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    path := s.configPath()
    b, err := ioutil.ReadFile(path)
    if err != nil {
        if os.IsNotExist(err) {
            s.cfg = &SyncConfigFile{Watches: make(map[string]*WatchEntry)}
            return nil
        }
        return err
    }
    cfg := &SyncConfigFile{}
    if err := json.Unmarshal(b, cfg); err != nil {
        return err
    }
    if cfg.Watches == nil {
        cfg.Watches = make(map[string]*WatchEntry)
    }
    s.cfg = cfg
    return nil
}

func (s *syncManager) save() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.cfg == nil {
        s.cfg = &SyncConfigFile{Watches: make(map[string]*WatchEntry)}
    }
    b, err := json.MarshalIndent(s.cfg, "", "  ")
    if err != nil {
        return err
    }
    return ioutil.WriteFile(s.configPath(), b, 0644)
}

func (s *syncManager) AddWatch(local, remote string, interval int, key, method string) error {
    return s.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, "", false, false)
}

func (s *syncManager) AddWatchWithIgnore(local, remote string, interval int, key, method, ignoreFile string) error {
    return s.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, false, false)
}

func (s *syncManager) AddWatchWithIgnoreAndRandom(local, remote string, interval int, key, method, ignoreFile string, randomFilename bool, merge bool) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    local = filepath.Clean(local)
    id := s.watchID(local)
    if _, ok := s.cfg.Watches[id]; ok {
        return fmt.Errorf("watch already exists for %s", local)
    }
    we := &WatchEntry{
        Local: local,
        Remote: remote,
        Interval: interval,
        Key: key,
        Method: method,
        IgnoreFile: ignoreFile,
        RandomFilename: randomFilename,
        Merge: merge,
        Files: make(map[string]syncFileState),
        FileNameMap: make(map[string]string),
    }
    s.cfg.Watches[id] = we
    return s.save()
}

func (s *syncManager) DeleteWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    if _, ok := s.cfg.Watches[id]; !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    // stop if running
    if s.cfg.Watches[id].Running && s.cfg.Watches[id].stopCh != nil {
        close(s.cfg.Watches[id].stopCh)
    }
    delete(s.cfg.Watches, id)
    return s.save()
}

func (s *syncManager) ListWatches() ([]*WatchEntry, error) {
    if err := s.load(); err != nil {
        return nil, err
    }
    res := make([]*WatchEntry, 0, len(s.cfg.Watches))
    for _, w := range s.cfg.Watches {
        res = append(res, w)
    }
    return res, nil
}

func (s *syncManager) StartWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    w, ok := s.cfg.Watches[id]
    if !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    if w.Running {
        return fmt.Errorf("watch already running: %s", local)
    }
    w.stopCh = make(chan struct{})
    w.Running = true
    go s.runWatch(w)
    fmt.Printf("开始执行任务 本地目录 %s 同步 (interval=%d)\n", w.Local, w.Interval)
    return s.save()
}

func (s *syncManager) StopWatch(local string) error {
    mgrMu.Lock()
    defer mgrMu.Unlock()
    if err := s.load(); err != nil {
        return err
    }
    id := s.watchID(filepath.Clean(local))
    w, ok := s.cfg.Watches[id]
    if !ok {
        return fmt.Errorf("watch not found: %s", local)
    }
    if !w.Running {
        return fmt.Errorf("watch not running: %s", local)
    }
    if w.stopCh != nil {
        close(w.stopCh)
    }
    w.Running = false
    w.stopCh = nil
    return s.save()
}

func (s *syncManager) StartAll() error {
    if err := s.load(); err != nil {
        return err
    }
    for _, w := range s.cfg.Watches {
        if !w.Running {
            w.stopCh = make(chan struct{})
            w.Running = true
            go s.runWatch(w)
            fmt.Printf("开始执行任务 本地目录 %s 同步 (interval=%d)\n", w.Local, w.Interval)
        }
    }
    return s.save()
}

func (s *syncManager) StopAll() error {
    if err := s.load(); err != nil {
        return err
    }
    for _, w := range s.cfg.Watches {
        if w.Running && w.stopCh != nil {
            close(w.stopCh)
            w.Running = false
            w.stopCh = nil
        }
    }
    return s.save()
}

func (s *syncManager) runWatch(w *WatchEntry) {
    // run loop
    ticker := time.NewTicker(time.Duration(w.Interval) * time.Second)
    defer ticker.Stop()
    // immediate run
    if w.Merge {
        s.mergeAndUpload(w)
    } else {
        s.scanAndUpload(w)
    }
    for {
        select {
        case <-ticker.C:
            if w.Merge {
                s.mergeAndUpload(w)
            } else {
                s.scanAndUpload(w)
            }
        case <-w.stopCh:
            return
        }
    }
}

// createZipFromDir 将目录打包为临时 zip 文件，返回 zip 文件路径
func createZipFromDir(dir string) (string, error) {
    tmpFile, err := ioutil.TempFile("", "pcs_merge_*.zip")
    if err != nil {
        return "", err
    }
    tmpPath := tmpFile.Name()
    tmpFile.Close()

    zipf, err := os.Create(tmpPath)
    if err != nil {
        return "", err
    }
    defer zipf.Close()

    zw := zip.NewWriter(zipf)
    defer zw.Close()

    err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        // skip directories
        if info.IsDir() {
            return nil
        }
        rel, err := filepath.Rel(dir, p)
        if err != nil {
            return err
        }
        // use forward slash in zip
        rel = filepath.ToSlash(rel)
        fh, err := zip.FileInfoHeader(info)
        if err != nil {
            return err
        }
        fh.Name = rel
        fh.Method = zip.Deflate
        w, err := zw.CreateHeader(fh)
        if err != nil {
            return err
        }
        rf, err := os.Open(p)
        if err != nil {
            return err
        }
        defer rf.Close()
        _, err = io.Copy(w, rf)
        return err
    })
    if err != nil {
        // remove tmp file on error
        os.Remove(tmpPath)
        return "", err
    }
    return tmpPath, nil
}

// mergeAndUpload 每次将目录打包并上传
func (s *syncManager) mergeAndUpload(w *WatchEntry) {
    zipPath, err := createZipFromDir(w.Local)
    if err != nil {
        fmt.Printf("打包 %s 失败: %s\n", w.Local, err)
        return
    }
    defer func() {
        if fileExists(zipPath) {
            os.Remove(zipPath)
        }
    }()

    uploadPath := zipPath

    // 如果启用了加密，则加密到指定输出路径
    if w.Key != "" {
        // 如果启用了随机文件名，确保生成并持久化该随机名
        if w.RandomFilename {
            if w.MergeFilename == "" {
                w.MergeFilename = uuid.New().String()
                // persist
                if err := s.save(); err != nil {
                    fmt.Printf("保存 sync 配置失败: %s\n", err)
                }
            }
        }

        var outPath string
        if w.RandomFilename && w.MergeFilename != "" {
            outPath = filepath.Join(filepath.Dir(zipPath), w.MergeFilename)
        } else {
            outPath = zipPath + ".encrypted"
        }
        if err := encryptFileForSync(zipPath, outPath, w.Key, w.Method); err != nil {
            fmt.Printf("加密失败: %s\n", err)
            return
        }
        uploadPath = outPath
    } else {
        // 未加密且启用了随机文件名无效；随机名仅在加密时生效（与现有行为保持一致）
    }

    // 计算保存路径（上传到远程目录）
    savePath := w.Remote
    fmt.Printf("[sync-merge] %s -> %s\n", uploadPath, savePath)
    RunUpload([]string{uploadPath}, savePath, &UploadOptions{})

    // 删除临时加密文件
    if uploadPath != zipPath {
        os.Remove(uploadPath)
    }

    next := time.Now().Add(time.Duration(w.Interval) * time.Second).Format(time.RFC3339)
    fmt.Printf("%s 合并上传完成, 下次上传时间为 %s\n", w.Local, next)
}

func (s *syncManager) scanAndUpload(w *WatchEntry) {
    walked, err := pcsutil.WalkDir(w.Local, "")
    if err != nil {
        fmt.Printf("walk %s error: %s\n", w.Local, err)
        return
    }
    for _, f := range walked {
        info, err := os.Stat(f)
        if err != nil || info.IsDir() {
            continue
        }
        fileUnix := filepath.ToSlash(filepath.Clean(f))
        baseUnix := filepath.ToSlash(w.Local)
        rel := strings.TrimPrefix(fileUnix, baseUnix)
        rel = strings.TrimPrefix(rel, "/")
        mod := info.ModTime().Unix()
        size := info.Size()

        // 计算文件的MD5
        currentMD5, err := md5sum(f)
        if err != nil {
            fmt.Printf("计算文件 %s 的MD5失败: %s, 跳过\n", rel, err)
            continue
        }

        prev, ok := w.Files[rel]
        if ok {
            if prev.MD5 == currentMD5 {
                continue
            }
            fmt.Printf("文件 %s 的MD5与配置中不一致: prev_md5=%s new_md5=%s, 执行上传\n", rel, prev.MD5, currentMD5)
        } else {
            fmt.Printf("文件 %s 未在配置中, 执行首次上传\n", rel)
        }

        // check ignore rules
        if shouldIgnore(w, rel, info.IsDir()) {
            continue
        }
        // prepare upload
        uploadPath := f
        uploadFileName := filepath.Base(rel)
        if w.Key != "" {
            tmp := f + ".encrypted"
            if err := encryptFileForSync(f, tmp, w.Key, w.Method); err != nil {
                fmt.Printf("encrypt error %s: %s\n", f, err)
                continue
            }
            uploadPath = tmp
                // 如果启用了随机文件名，使用已有映射或生成并保存一个固定的 UUID
                if w.RandomFilename {
                    if w.FileNameMap == nil {
                        w.FileNameMap = make(map[string]string)
                    }
                    // 按相对路径查找已存在的随机名
                    if existing, ok := w.FileNameMap[rel]; ok && existing != "" {
                        uploadFileName = existing
                    } else {
                        newFileName := uuid.New().String()
                        uploadFileName = newFileName
                        w.FileNameMap[rel] = newFileName
                        fmt.Printf("[sync] 生成随机文件名映射: %s -> %s\n", newFileName, rel)
                    }
                }
        }
        relDir := filepath.Dir(rel)
        var savePath string
        if relDir == "." {
            savePath = w.Remote
        } else {
            savePath = path.Clean(w.Remote + baidupcs.PathSeparator + filepath.ToSlash(relDir))
        }
        
        // 如果启用了随机文件名，创建临时文件并重命名
        if w.RandomFilename && w.Key != "" && uploadFileName != filepath.Base(rel) {
            tempUploadPath := filepath.Join(filepath.Dir(uploadPath), uploadFileName)
            if err := os.Rename(uploadPath, tempUploadPath); err != nil {
                fmt.Printf("rename error %s -> %s: %s\n", uploadPath, tempUploadPath, err)
                continue
            }
            uploadPath = tempUploadPath
        }
        
        fmt.Printf("[sync] %s -> %s\n", uploadPath, savePath)
        RunUpload([]string{uploadPath}, savePath, &UploadOptions{})
        // remove temporary encrypted file if any
        if uploadPath != f {
            os.Remove(uploadPath)
        }
        // update state
        if w.Files == nil {
            w.Files = make(map[string]syncFileState)
        }
        w.Files[rel] = syncFileState{ModTime: mod, Size: size, MD5: currentMD5}
        // persist
        if err := s.save(); err != nil {
            fmt.Printf("save sync config error: %s\n", err)
        }
    }
    // 完成本次扫描/上传
    next := time.Now().Add(time.Duration(w.Interval) * time.Second).Format(time.RFC3339)
    fmt.Printf("%s 同步完成, 下次同步时间为 %s\n", w.Local, next)
}

func (s *syncManager) watchID(local string) string {
    // use sha1 of absolute path as id
    h := sha1.Sum([]byte(local))
    return hex.EncodeToString(h[:])
}

// helper wrappers used by main
func AddSyncWatch(local, remote string, interval int, key, method string) error {
    return mgr.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, "", false, false)
}

// AddSyncWatchWithIgnore allows specifying an ignore file path (relative to local or absolute)
func AddSyncWatchWithIgnore(local, remote string, interval int, key, method, ignoreFile string) error {
    return mgr.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, false, false)
}

// AddSyncWatchWithIgnoreAndRandom allows specifying an ignore file path and random filename option
func AddSyncWatchWithIgnoreAndRandom(local, remote string, interval int, key, method, ignoreFile string, randomFilename bool) error {
    return mgr.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, randomFilename, false)
}

// AddSyncWatchWithIgnoreAndRandomAndMerge allows specifying ignore, random filename and merge mode
func AddSyncWatchWithIgnoreAndRandomAndMerge(local, remote string, interval int, key, method, ignoreFile string, randomFilename bool, merge bool) error {
    return mgr.AddWatchWithIgnoreAndRandom(local, remote, interval, key, method, ignoreFile, randomFilename, merge)
}

func DeleteSyncWatch(local string) error {
    return mgr.DeleteWatch(local)
}

func ListSyncWatches() ([]*WatchEntry, error) {
    return mgr.ListWatches()
}

func StartSync(local string) error {
    if local == "" {
        if err := mgr.StartAll(); err != nil {
            return err
        }
    } else {
        if err := mgr.StartWatch(local); err != nil {
            return err
        }
    }

    // Block and wait for interrupt/terminate signal, then stop watches
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
    <-sigs

    if local == "" {
        return mgr.StopAll()
    }
    return mgr.StopWatch(local)
}

func StopSync(local string) error {
    if local == "" {
        return mgr.StopAll()
    }
    return mgr.StopWatch(local)
}
